package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/telemetry"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestDaemonLivenessWatchdog_RecoversFromTransientFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Two failures (< threshold 3), then a success (resets streak), then two
		// more failures, then cancel on the 6th probe. The watchdog must never trip.
		outcomes := []error{
			errors.New("timeout 1"),
			errors.New("timeout 2"),
			nil,
			errors.New("timeout 3"),
			errors.New("timeout 4"),
		}
		var calls atomic.Int32
		var exited atomic.Bool
		var logBuf bytes.Buffer

		done := make(chan struct{})
		go func() {
			defer close(done)
			runDaemonLivenessWatchdog(ctx, daemonLivenessConfig{
				Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
				Interval:         defaultDaemonLivenessInterval,
				ProbeTimeout:     defaultDaemonLivenessProbeTimeout,
				FailureThreshold: defaultDaemonLivenessFailureThreshold,
				Diagnostics:      &logBuf,
				Probe: func(context.Context) error {
					idx := int(calls.Add(1)) - 1
					if idx < len(outcomes) {
						return outcomes[idx]
					}
					cancel()
					return context.Canceled
				},
				Exit: func(int) { exited.Store(true) },
			})
		}()

		<-done
		assert.False(t, exited.Load(), "transient failures below threshold must not exit")
		assert.Empty(t, logBuf.String(), "no stack dump should be written on recovered failures")
		assert.GreaterOrEqual(t, int(calls.Load()), len(outcomes)+1)
	})
}

func TestDaemonLivenessWatchdog_TripsOnWedgedListenerAndDumpsStacks(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	// Accept connections without ever writing HTTP headers, reproducing a
	// wedged serve loop where the kernel accepts connections yet /api/v1/ping
	// times out awaiting headers.
	var acceptedMu sync.Mutex
	var accepted []net.Conn
	defer func() {
		acceptedMu.Lock()
		for _, c := range accepted {
			_ = c.Close()
		}
		acceptedMu.Unlock()
	}()
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			acceptedMu.Lock()
			accepted = append(accepted, conn)
			acceptedMu.Unlock()
		}
	}()

	var logBuf bytes.Buffer
	exitCode := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDaemonLivenessWatchdog(t.Context(), daemonLivenessConfig{
			Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: ln.Addr().String()},
			Interval:         10 * time.Millisecond,
			ProbeTimeout:     40 * time.Millisecond, //nolint:kennlint // deadlinetest: wedged listener holds connections open so probe timeout is the asserted failure
			FailureThreshold: 3,
			Diagnostics:      &logBuf,
			Exit:             func(code int) { exitCode <- code },
		})
	}()

	select {
	case code := <-exitCode:
		assert.Equal(t, ExitInternal, code)
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not trip on wedged listener")
	}
	<-done

	dump := logBuf.String()
	assert.Contains(t, dump, "stopped answering /api/v1/ping")
	assert.Contains(t, dump, "3 consecutive failures")
	assert.Contains(t, dump, "goroutine ")
}

func TestDaemonLivenessWatchdog_DoesNotTripOnIntentionalShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	probeStarted := make(chan struct{})
	var exited atomic.Bool
	var logBuf bytes.Buffer

	done := make(chan struct{})
	go func() {
		defer close(done)
		runDaemonLivenessWatchdog(ctx, daemonLivenessConfig{
			Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
			Interval:         5 * time.Millisecond,
			ProbeTimeout:     time.Second,
			FailureThreshold: 1,
			Diagnostics:      &logBuf,
			Probe: func(probeCtx context.Context) error {
				close(probeStarted)
				<-probeCtx.Done()
				return errors.New("connection closed during shutdown")
			},
			Exit: func(int) { exited.Store(true) },
		})
	}()

	<-probeStarted
	cancel()
	<-done

	assert.False(t, exited.Load(), "intentional shutdown must not trigger watchdog exit")
	assert.Empty(t, logBuf.String(), "intentional shutdown must not dump watchdog stacks")
}

type cancellingDiagnosticWriter struct {
	cancel context.CancelFunc
	buf    bytes.Buffer
}

func (w *cancellingDiagnosticWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	w.cancel()
	return n, err
}

func TestDaemonLivenessWatchdog_SkipsExitWhenShutdownCancelsDuringReport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		writer := &cancellingDiagnosticWriter{cancel: cancel}
		var beforeExitCalled atomic.Bool
		var exited atomic.Bool

		done := make(chan struct{})
		go func() {
			defer close(done)
			runDaemonLivenessWatchdog(ctx, daemonLivenessConfig{
				Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
				Interval:         defaultDaemonLivenessInterval,
				ProbeTimeout:     defaultDaemonLivenessProbeTimeout,
				FailureThreshold: 1,
				Diagnostics:      writer,
				Probe:            func(context.Context) error { return errors.New("probe failed") },
				BeforeExit:       func() { beforeExitCalled.Store(true) },
				Exit:             func(int) { exited.Store(true) },
			})
		}()

		<-done
		assert.False(t, beforeExitCalled.Load(), "shutdown starting during report must skip runtime cleanup")
		assert.False(t, exited.Load(), "shutdown starting during report must skip forced exit")
	})
}

func TestDaemonLivenessWatchdog_DetectsPIDMismatch(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.MarshalWrite(w, client.PingInfo{
				OK:      true,
				Service: "kata",
				Version: "test",
				PID:     os.Getpid() + 1,
			})
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	var logBuf bytes.Buffer
	exitCode := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDaemonLivenessWatchdog(t.Context(), daemonLivenessConfig{
			Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: ln.Addr().String()},
			Interval:         5 * time.Millisecond,
			ProbeTimeout:     time.Second,
			FailureThreshold: 2,
			Diagnostics:      &logBuf,
			Exit:             func(code int) { exitCode <- code },
		})
	}()

	select {
	case code := <-exitCode:
		assert.Equal(t, ExitInternal, code)
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not trip on PID mismatch")
	}
	<-done
	assert.Contains(t, logBuf.String(), "expected")
}

func TestDaemonLivenessWatchdog_RetainsDumpAndExitsWhenStderrBlocked(t *testing.T) {
	var retained bytes.Buffer
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	writer := daemonDiagnosticWriter{
		stderr: blockingDiagnosticWriter{entered: entered, release: release},
		file:   &retained,
	}
	exitCode := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDaemonLivenessWatchdog(t.Context(), daemonLivenessConfig{
			Endpoint:         kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: "127.0.0.1:1"},
			Interval:         5 * time.Millisecond,
			ProbeTimeout:     time.Second,
			FailureThreshold: 1,
			Diagnostics:      writer,
			Probe:            func(context.Context) error { return errors.New("probe failed") },
			Exit:             func(code int) { exitCode <- code },
		})
	}()

	select {
	case code := <-exitCode:
		assert.Equal(t, ExitInternal, code)
	case <-time.After(5 * time.Second):
		t.Fatal("blocked stderr prevented watchdog from exiting")
	}
	<-done
	assert.Contains(t, retained.String(), "stopped answering /api/v1/ping")
	assert.Contains(t, retained.String(), "goroutine ")
}

type countingIdleAdmission struct {
	inner daemon.IdleForegroundAdmission
	calls atomic.Int32
}

func (a *countingIdleAdmission) TryForeground() (*daemon.IdleLease, bool) {
	a.calls.Add(1)
	return a.inner.TryForeground()
}

func TestDaemonLivenessProbe_RealServerSocketAndSharedTCPWithoutResettingIdle(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	t.Run("unix socket does not acquire foreground idle lease", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix socket listener test is Unix-only")
		}
		sockDir, err := os.MkdirTemp("/tmp", "kata-live-")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
		sockPath := filepath.Join(sockDir, "d.sock")
		ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sockPath)
		require.NoError(t, err)
		defer func() { _ = ln.Close() }()

		idle := daemon.NewIdleController(time.Hour, func() {})
		defer idle.Stop()
		admission := &countingIdleAdmission{inner: idle}
		srv := daemon.NewServer(daemon.ServerConfig{
			DB:            store,
			StartedAt:     time.Now().UTC(),
			IdleAdmission: admission,
		})
		defer func() { _ = srv.Close() }()

		serveCtx, serveCancel := context.WithCancel(t.Context())
		defer serveCancel()
		serveDone := make(chan error, 1)
		go func() {
			serveDone <- srv.ServeListeners(serveCtx, daemon.ListenerBinding{
				Listener: ln,
				Policy:   daemon.ListenerPolicy{Kind: daemon.ListenerSocket},
			})
		}()
		idle.Start()

		probe, err := newDaemonLivenessProbe(
			kitdaemon.Endpoint{Network: kitdaemon.NetworkUnix, Address: sockPath},
		)
		require.NoError(t, err)

		probeCtx, probeCancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer probeCancel()
		for range 3 {
			require.NoError(t, probe(probeCtx))
		}
		assert.Zero(t, admission.calls.Load(), "liveness self-probes must be observational and never reset idle shutdown")

		// Verify a non-probe API call on the same socket does acquire a foreground lease.
		unixClient := &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
				},
			},
		}
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, client.UnixBase+"/api/v1/projects", nil)
		require.NoError(t, err)
		resp, err := unixClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, int32(1), admission.calls.Load(), "ordinary API requests must still acquire foreground leases")

		serveCancel()
		require.NoError(t, <-serveDone)
	})

	t.Run("shared wildcard TCP with auth and web sessions", func(t *testing.T) {
		ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "0.0.0.0:0")
		require.NoError(t, err)
		defer func() { _ = ln.Close() }()
		_, port, err := net.SplitHostPort(ln.Addr().String())
		require.NoError(t, err)
		runtimeAddr := net.JoinHostPort("0.0.0.0", port)

		authCfg := config.AuthConfig{
			Token:                "secret-daemon-token",
			RequireTokenIdentity: true,
		}
		webSessions, err := daemon.NewWebSessionManager(daemon.WebSessionManagerConfig{
			Origin:       "http://127.0.0.1:" + port,
			OriginStable: true,
			InstanceID:   "testinstance1",
			Writable:     true,
			Updates:      "sse",
			Auth:         authCfg,
			DB:           store,
		})
		require.NoError(t, err)

		srv := daemon.NewServer(daemon.ServerConfig{
			DB:          store,
			StartedAt:   time.Now().UTC(),
			Auth:        authCfg,
			WebSessions: webSessions,
		})
		defer func() { _ = srv.Close() }()

		serveCtx, serveCancel := context.WithCancel(t.Context())
		defer serveCancel()
		serveDone := make(chan error, 1)
		go func() {
			serveDone <- srv.ServeListeners(serveCtx, daemon.ListenerBinding{
				Listener: ln,
				Policy: daemon.ListenerPolicy{
					Kind:                  daemon.ListenerSharedTCP,
					Origin:                "http://127.0.0.1:" + port,
					BackendAuthority:      runtimeAddr,
					RequireBrowserSession: true,
					WebAuthentication:     "login",
				},
			})
		}()

		probe, err := newDaemonLivenessProbe(
			kitdaemon.Endpoint{Network: kitdaemon.NetworkTCP, Address: runtimeAddr},
		)
		require.NoError(t, err)

		probeCtx, probeCancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer probeCancel()
		require.NoError(t, probe(probeCtx))

		serveCancel()
		require.NoError(t, <-serveDone)
	})
}

type wedgingListener struct {
	net.Listener
	wedge atomic.Bool
	mu    sync.Mutex
	held  []net.Conn
}

func (l *wedgingListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if !l.wedge.Load() {
			return conn, nil
		}
		// Hold the accepted connection open without ever returning it to http.Server,
		// reproducing a listener whose socket accepts connections while /api/v1/ping
		// times out awaiting headers.
		l.mu.Lock()
		l.held = append(l.held, conn)
		l.mu.Unlock()
	}
}

func (l *wedgingListener) Close() error {
	err := l.Listener.Close()
	l.mu.Lock()
	for _, c := range l.held {
		_ = c.Close()
	}
	l.held = nil
	l.mu.Unlock()
	return err
}

func TestDaemonLivenessWatchdogProcessHelper(t *testing.T) {
	if os.Getenv("KATA_LIVENESS_WATCHDOG_HELPER") != "1" {
		return
	}
	originalTelemetry := newTelemetryReporter
	newTelemetryReporter = func(telemetry.Options) telemetry.Client { return &fakeTelemetryReporter{} }
	t.Cleanup(func() { newTelemetryReporter = originalTelemetry })

	originalInterval := daemonLivenessInterval
	originalTimeout := daemonLivenessProbeTimeout
	originalThreshold := daemonLivenessFailureThreshold
	originalListen := listenPrimaryDaemon
	t.Cleanup(func() {
		daemonLivenessInterval = originalInterval
		daemonLivenessProbeTimeout = originalTimeout
		daemonLivenessFailureThreshold = originalThreshold
		listenPrimaryDaemon = originalListen
	})

	daemonLivenessInterval = 25 * time.Millisecond
	daemonLivenessProbeTimeout = 60 * time.Millisecond
	daemonLivenessFailureThreshold = 2

	wedged := &wedgingListener{}
	listenPrimaryDaemon = func(ctx context.Context, endpoint kitdaemon.Endpoint, opts ...kitdaemon.ListenOption) (net.Listener, error) {
		ln, err := kitdaemon.Listen(ctx, endpoint, opts...)
		if err != nil {
			return nil, err
		}
		wedged.Listener = ln
		return wedged, nil
	}

	// Wedge the listener as soon as the runtime record is published.
	go func() {
		ns, err := daemonruntime.NewNamespace()
		if err != nil {
			return
		}
		store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			records, listErr := store.List()
			if listErr == nil && len(records) > 0 {
				wedged.wedge.Store(true)
				return
			}
			select {
			case <-ticker.C:
			case <-deadline.C:
				return
			}
		}
	}()

	err := runDaemonWithListen(t.Context(), "", false, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

type observingListener struct {
	net.Listener
	accepted atomic.Int32
	notify   chan struct{}
}

func (l *observingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
		select {
		case l.notify <- struct{}{}:
		default:
		}
	}
	return conn, err
}

func TestDaemonLivenessWatchdog_DoesNotTripDuringSlowStartup(t *testing.T) {
	resetFlags(t)
	setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	t.Setenv("PORT", "")

	originalInterval := daemonLivenessInterval
	originalTimeout := daemonLivenessProbeTimeout
	originalThreshold := daemonLivenessFailureThreshold
	originalTelemetry := newTelemetryReporter
	originalListen := listenPrimaryDaemon
	t.Cleanup(func() {
		daemonLivenessInterval = originalInterval
		daemonLivenessProbeTimeout = originalTimeout
		daemonLivenessFailureThreshold = originalThreshold
		newTelemetryReporter = originalTelemetry
		listenPrimaryDaemon = originalListen
	})

	// Configure a short watchdog budget (2 * (15ms + 15ms) = 60ms) and stall
	// startup after the listener is bound (before onReady) for 150ms. The
	// watchdog must not start until readiness, so slow startup/migrations
	// cannot false-trigger it.
	daemonLivenessInterval = 15 * time.Millisecond
	daemonLivenessProbeTimeout = 15 * time.Millisecond
	daemonLivenessFailureThreshold = 2

	observed := &observingListener{notify: make(chan struct{}, 8)}
	listenPrimaryDaemon = func(ctx context.Context, endpoint kitdaemon.Endpoint, opts ...kitdaemon.ListenOption) (net.Listener, error) {
		ln, err := kitdaemon.Listen(ctx, endpoint, opts...)
		if err != nil {
			return nil, err
		}
		observed.Listener = ln
		return observed, nil
	}

	startupStalled := make(chan struct{})
	releaseStartup := make(chan struct{})
	newTelemetryReporter = func(telemetry.Options) telemetry.Client {
		close(startupStalled)
		<-releaseStartup
		return &fakeTelemetryReporter{}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var diagnostics bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemonProcess(ctx, "127.0.0.1:0", false, true, nil, &diagnostics)
	}()

	select {
	case <-startupStalled:
	case err := <-done:
		t.Fatalf("daemon exited before startup stall: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not reach pre-readiness startup hook")
	}

	// Hold startup past multiple watchdog intervals while the listener is bound
	// but not yet serving.
	stallTimer := time.NewTimer(150 * time.Millisecond) //nolint:kennlint // deadlinetest: absence window; startup stalls past the watchdog budget to prove probing does not begin before readiness
	select {
	case <-stallTimer.C:
	case err := <-done:
		stallTimer.Stop()
		t.Fatalf("daemon exited prematurely during slow startup: %v", err)
	}
	close(releaseStartup)

	// Wait for at least two real watchdog probes to be accepted after readiness.
	deadline := time.After(5 * time.Second)
	for observed.accepted.Load() < 2 {
		select {
		case <-observed.notify:
		case err := <-done:
			t.Fatalf("daemon exited unexpectedly after becoming ready: %v", err)
		case <-deadline:
			t.Fatal("watchdog did not begin probing after daemon readiness")
		}
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not shut down cleanly")
	}
	assert.NotContains(t, diagnostics.String(), "stopped answering /api/v1/ping")
}

func TestDaemonLivenessWatchdog_SubprocessDumpsToDaemonLogAndExitsNonzero(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	t.Setenv("PORT", "")
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(ns.DataDir, 0o700))

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDaemonLivenessWatchdogProcessHelper$") // #nosec G204 G702 -- rerun the current test binary with a fixed selector
	child.Env = append(os.Environ(), "KATA_LIVENESS_WATCHDOG_HELPER=1")
	// Leave Stderr nil so Unix launches get /dev/null and startDaemonLogging
	// redirects stderr into daemon.log just like launchd/systemd.
	err = child.Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "watchdog must exit nonzero when the daemon listener wedges")
	assert.Equal(t, ExitInternal, exitErr.ExitCode())

	logPath := filepath.Join(ns.DataDir, "daemon.log")
	contents, readErr := os.ReadFile(logPath) // #nosec G304 -- test-owned daemon.log under t.TempDir
	require.NoError(t, readErr)
	assert.Contains(t, string(contents), "stopped answering /api/v1/ping")
	assert.Contains(t, string(contents), "2 consecutive failures")
	assert.Contains(t, string(contents), "a service supervisor will restart it, or the next kata command will start a new daemon")
	assert.Contains(t, string(contents), "goroutine ")
	assert.Contains(t, string(contents), "runDaemonLivenessWatchdog")

	records, listErr := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
	require.NoError(t, listErr)
	assert.Empty(t, records, "runtime record must be cleaned up before watchdog exits")
}
