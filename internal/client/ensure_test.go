package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestEnsureRunningRestartsWhenDaemonVersionDiffers(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)

	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
		"pid":     os.Getpid(),
	})

	require.NoError(t, writeRuntimeRecord(t, tmp, addr))
	restore := patchEnsureHooks(t, "new-version", "http://new-daemon")
	url, err := EnsureRunning(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "http://new-daemon", url)
	assert.Equal(t, 1, restore.stopCalls)
	assert.Equal(t, 1, restore.startCalls)
}

func TestEnsureRunningRestartsWhenDaemonVersionUnknown(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)

	_, addr := startMockDaemonPing(t, map[string]any{"ok": true})

	require.NoError(t, writeRuntimeRecord(t, tmp, addr))
	restore := patchEnsureHooks(t, "new-version", "http://new-daemon")
	url, err := EnsureRunning(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "http://new-daemon", url)
	assert.Equal(t, 1, restore.stopCalls)
	assert.Equal(t, 1, restore.startCalls)
}

func TestEnsureLocalRunningIgnoresRemoteOverride(t *testing.T) {
	t.Setenv("KATA_SERVER", "http://100.64.0.5:7777")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")
	setupKataEnv(t)
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://local-daemon")

	url, err := EnsureLocalRunning(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "http://local-daemon", url)
	assert.Equal(t, 1, restore.startCalls)
}

func TestEnsureRunningTargetMarksEnvironmentRemote(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", srv.URL)

	target, err := EnsureRunningTarget(t.Context())

	require.NoError(t, err)
	assert.Equal(t, srv.URL, target.BaseURL)
	assert.True(t, target.ConfiguredRemote)
}

func TestEnsureRunningTargetMarksWorkspaceConfigRemote(t *testing.T) {
	srv := pingingServer(t)
	t.Setenv("KATA_SERVER", "")
	workspace := t.TempDir()
	t.Chdir(workspace)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.toml"),
		[]byte("version = 1\n\n[project]\nidentity = \"example.test/spoke-project\"\nname = \"spoke-project\"\n"),
		0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".kata.local.toml"),
		[]byte("version = 1\n\n[server]\nurl = \""+srv.URL+"\"\n"),
		0o600))

	target, err := EnsureRunningTarget(t.Context())

	require.NoError(t, err)
	assert.Equal(t, srv.URL, target.BaseURL)
	assert.True(t, target.ConfiguredRemote)
}

func TestEnsureRunningTargetMarksLocalDiscovery(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	setupKataEnv(t)
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://local-daemon")

	target, err := EnsureRunningTarget(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "http://local-daemon", target.BaseURL)
	assert.False(t, target.ConfiguredRemote)
	assert.Equal(t, 1, restore.startCalls)
}

func TestEnsureRunningTargetRetainsSelectedLocalEndpoint(t *testing.T) {
	t.Setenv("KATA_SERVER", "")
	home := setupKataEnv(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok": true, "service": "kata", "version": currentVersionForEnsure(),
	})
	require.NoError(t, writeRuntimeRecord(t, home, addr))

	target, err := EnsureRunningTarget(t.Context())

	require.NoError(t, err)
	assert.Equal(t, addr, target.Address)
	assert.Equal(t, "tcp", target.Network)
	assert.Equal(t, "http", target.Scheme)
	assert.Equal(t, "http://"+addr, target.BaseURL)
}

func TestDiscoverForEnsureThreeOutcomes(t *testing.T) {
	tests := []struct {
		name        string
		pingPayload map[string]any
		want        daemonScanOutcome
	}{
		{name: "no live record", want: daemonScanNone},
		{
			name: "live and compatible",
			pingPayload: map[string]any{
				"ok": true, "service": "kata", "version": "test-version", "pid": os.Getpid(),
			},
			want: daemonScanCompatible,
		},
		{
			name: "live but stale build",
			pingPayload: map[string]any{
				"ok": true, "service": "kata", "version": "old-version", "pid": os.Getpid(),
			},
			want: daemonScanStale,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
			tmp := setupKataEnv(t)
			origVersion := currentVersionForEnsure
			currentVersionForEnsure = func() string { return "test-version" }
			t.Cleanup(func() { currentVersionForEnsure = origVersion })

			wantURL := ""
			wantAddress := ""
			if tt.pingPayload != nil {
				wantURL, wantAddress = startMockDaemonPing(t, tt.pingPayload)
				require.NoError(t, writeRuntimeRecord(t, tmp, wantAddress))
			}
			ns, err := daemon.NewNamespace()
			require.NoError(t, err)

			got, err := discoverForEnsure(context.Background(), ns.DataDir)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Outcome)
			if tt.want == daemonScanNone {
				assert.Empty(t, got.Daemon.BaseURL)
				return
			}
			assert.Equal(t, wantURL, got.Daemon.BaseURL)
			assert.Equal(t, wantAddress, got.Daemon.Record.Endpoint().ConfigAddress())
			assert.Equal(t, os.Getpid(), got.Daemon.Record.PID)
		})
	}
}

func TestDiscoverForEnsurePreservesScanErrors(t *testing.T) {
	t.Run("runtime store", func(t *testing.T) {
		_, err := discoverForEnsure(t.Context(), "relative-data-dir")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be absolute")
	})

	t.Run("context cancellation", func(t *testing.T) {
		tmp := setupKataEnv(t)
		require.NoError(t, writeRuntimeRecord(t, tmp, "unix://"+filepath.Join(tmp, "missing.sock")))
		ns, err := daemon.NewNamespace()
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err = discoverForEnsure(ctx, ns.DataDir)

		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestDiscoverWebRuntime(t *testing.T) {
	record := kitdaemon.RuntimeRecord{
		PID: 4242,
		Metadata: map[string]string{
			"web_origin":        "http://127.0.0.1:27123",
			"web_origin_stable": "true",
			"web_capabilities":  "loopback,sse",
		},
	}

	discovered, err := DiscoverWebRuntime(record)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:27123", discovered.Origin)
	assert.True(t, discovered.OriginStable)
	assert.Equal(t, []string{"loopback", "sse"}, discovered.Capabilities)
}

func TestAutoStartUsesKitDetachedStarter(t *testing.T) {
	setupKataEnv(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": currentVersionForEnsure(),
		"pid":     os.Getpid(),
	})

	orig := startDetachedDaemonForEnsure
	var got kitdaemon.StartDetachedOptions
	startDetachedDaemonForEnsure = func(ctx context.Context, opts kitdaemon.StartDetachedOptions) error {
		require.NoError(t, ctx.Err())
		got = opts
		_, err := fmt.Fprintln(opts.Stdout, "daemon stdout")
		require.NoError(t, err)
		_, err = fmt.Fprintln(opts.Stderr, "daemon stderr")
		require.NoError(t, err)
		_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
			PID:       os.Getpid(),
			Address:   addr,
			StartedAt: time.Now().UTC(),
		})
		return err
	}
	t.Cleanup(func() { startDetachedDaemonForEnsure = orig })

	target, err := autoStart(context.Background(), ns.DataDir)

	require.NoError(t, err)
	assert.Equal(t, "http://"+addr, target.BaseURL)
	assert.Equal(t, addr, target.Address)
	assert.Equal(t, []string{"daemon", "start", "--foreground"}, got.Args)
	assert.True(t, got.RefuseEphemeral)
	assert.Contains(t, got.Env, daemon.AutoStartMarkerEnv+"=1")

	logData, err := os.ReadFile(filepath.Join(ns.DataDir, "daemon.log"))
	require.NoError(t, err)
	assert.Contains(t, string(logData), "daemon stdout")
	assert.Contains(t, string(logData), "daemon stderr")
}

func TestAutoStartReportsRestrictedStateDirectoryBeforeSpawn(t *testing.T) {
	dataDir := setupKataEnv(t)
	originalCheckState := checkDaemonStateForEnsure
	originalStart := startDetachedDaemonForEnsure
	checkDaemonStateForEnsure = func(string) error {
		return os.ErrPermission
	}
	started := false
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		started = true
		return nil
	}
	t.Cleanup(func() {
		checkDaemonStateForEnsure = originalCheckState
		startDetachedDaemonForEnsure = originalStart
	})

	_, err := autoStart(t.Context(), dataDir)

	require.ErrorIs(t, err, os.ErrPermission)
	assert.Contains(t, err.Error(), "check filesystem permissions or sandbox access and retry")
	assert.Contains(t, err.Error(), dataDir)
	assert.False(t, started, "daemon should not be spawned when its state directory is inaccessible")
}

func TestAutoStartAllowsDaemonStartupBeyondFiveSeconds(t *testing.T) {
	dataDir := setupKataEnv(t)
	originalStart := startDetachedDaemonForEnsure
	originalDiscover := discoverDaemonForAutoStart
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		return nil
	}
	t.Cleanup(func() {
		startDetachedDaemonForEnsure = originalStart
		discoverDaemonForAutoStart = originalDiscover
	})

	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			if time.Since(startedAt) < 6*time.Second {
				return ensureDiscovery{}, nil
			}
			return compatibleEnsureDiscovery("127.0.0.1:27123"), nil
		}

		target, err := autoStart(t.Context(), dataDir)

		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:27123", target.BaseURL)
	})
}

func TestAutoStartWaitsForUnreachableSpawnedDaemonToBecomeReady(t *testing.T) {
	dataDir := setupKataEnv(t)
	originalStart := startDetachedDaemonForEnsure
	originalDiscover := discoverDaemonForAutoStart
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		return nil
	}
	t.Cleanup(func() {
		startDetachedDaemonForEnsure = originalStart
		discoverDaemonForAutoStart = originalDiscover
	})

	synctest.Test(t, func(t *testing.T) {
		startedAt := time.Now()
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			if time.Since(startedAt) < 6*time.Second {
				return ensureDiscovery{}, fmt.Errorf("%w: listener not ready", ErrLocalDaemonUnreachable)
			}
			return compatibleEnsureDiscovery("127.0.0.1:27123"), nil
		}

		target, err := autoStart(t.Context(), dataDir)

		require.NoError(t, err)
		assert.Equal(t, "http://127.0.0.1:27123", target.BaseURL)
	})
}

func TestAutoStartReportsPersistentUnreachableDaemonAfterReadinessDeadline(t *testing.T) {
	dataDir := setupKataEnv(t)
	originalStart := startDetachedDaemonForEnsure
	originalDiscover := discoverDaemonForAutoStart
	startDetachedDaemonForEnsure = func(context.Context, kitdaemon.StartDetachedOptions) error {
		return nil
	}
	t.Cleanup(func() {
		startDetachedDaemonForEnsure = originalStart
		discoverDaemonForAutoStart = originalDiscover
	})

	synctest.Test(t, func(t *testing.T) {
		probeErr := fmt.Errorf("%w: daemon pid 123 at unix:///tmp/kata.sock", ErrLocalDaemonUnreachable)
		discoverDaemonForAutoStart = func(context.Context, string) (ensureDiscovery, error) {
			return ensureDiscovery{}, probeErr
		}

		_, err := autoStart(t.Context(), dataDir)

		require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
		assert.ErrorIs(t, err, probeErr)
		assert.Contains(t, err.Error(), "daemon pid 123")
		assert.NotContains(t, err.Error(), "failed to start within")
	})
}

func TestStopRunningDaemonsDoesNotSignalUnverifiedRuntimePID(t *testing.T) {
	tmp := setupKataEnv(t)
	cmd, waitCh := startLongLivedTestProcess(t)

	require.NoError(t, writeRuntimeRecordForPID(t, tmp, cmd.Process.Pid, "127.0.0.1:1"))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, stopRunningDaemons(context.Background(), ns.DataDir, ns.DBHash))

	select {
	case err := <-waitCh:
		t.Fatalf("unverified runtime PID was signaled; process exited with %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestNewHTTPClientWithoutAuthSkipsDeadRuntimeRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	tmp := setupKataEnv(t)
	socket := filepath.Join(t.TempDir(), "kata.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/ping" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "kata"})
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// A dead PID pointing at a socket path that does not exist, plus the
	// live record. Construction must reach the live one.
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, deadPID(t), "unix:///nonexistent/kata.sock"))
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), "unix://"+socket))

	c, err := newHTTPClientWithoutAuth(context.Background(), UnixBase, Opts{Timeout: 2 * time.Second})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, UnixBase+"/api/v1/ping", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNewHTTPClientWithoutAuthSkipsLiveTCPRuntimeBeforeUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix sockets are unsupported on Windows")
	}
	tmp := setupKataEnv(t)
	_, tcpAddress := startMockDaemonPing(t, map[string]any{
		"ok": true, "service": "kata", "version": currentVersionForEnsure(),
	})
	tcpProcess, _ := startLongLivedTestProcess(t)
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, tcpProcess.Process.Pid, tcpAddress))

	socket := filepath.Join(t.TempDir(), "kata.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/ping":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true, "service": "kata", "version": currentVersionForEnsure(),
				})
			case "/ready":
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), "unix://"+socket))

	c, err := newHTTPClientWithoutAuth(t.Context(), UnixBase, Opts{Timeout: 2 * time.Second})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, UnixBase+"/ready", nil)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

// deadPID returns a PID that is not alive: a child process that has already
// been reaped.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestEnsureSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	cmd.Env = append(os.Environ(), "KATA_ENSURE_SLEEP_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	require.NoError(t, stdin.Close())
	_ = cmd.Wait()
	return pid
}

func startLongLivedTestProcess(t *testing.T) (*exec.Cmd, <-chan error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestEnsureSleepHelperProcess", "--") //nolint:gosec // test helper starts this test binary
	cmd.Env = append(os.Environ(), "KATA_ENSURE_SLEEP_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	waitCh := make(chan error, 1)
	waitDone := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(waitDone)
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			_ = cmd.Process.Kill()
			<-waitDone
		}
	})
	return cmd, waitCh
}

func TestEnsureSleepHelperProcess(_ *testing.T) {
	if os.Getenv("KATA_ENSURE_SLEEP_HELPER") != "1" {
		return
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestStopRunningDaemonsSignalsVerifiedIncompatibleRuntime(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
		"pid":     os.Getpid(),
	})
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), addr))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)

	origSignal := signalDaemonStopForEnsure
	var signaled kitdaemon.RuntimeRecord
	var signaledDBHash string
	signalDaemonStopForEnsure = func(rec kitdaemon.RuntimeRecord, dbhash string) error {
		signaled = rec
		signaledDBHash = dbhash
		return os.Remove(filepath.Join(ns.DataDir, fmt.Sprintf("daemon.%d.json", rec.PID)))
	}
	t.Cleanup(func() { signalDaemonStopForEnsure = origSignal })

	require.NoError(t, stopRunningDaemons(context.Background(), ns.DataDir, ns.DBHash))
	assert.Equal(t, os.Getpid(), signaled.PID)
	assert.Equal(t, ns.DBHash, signaledDBHash)
}

func TestStopRunningDaemonsReportsUnreachableDaemonRemainingAfterSignal(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)
	child, _ := startLongLivedTestProcess(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
		"pid":     child.Process.Pid,
	})
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, child.Process.Pid, addr))
	unreachableAddress := "unix://" + filepath.Join(tmp, "missing.sock")
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), unreachableAddress))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)

	origSignal := signalDaemonStopForEnsure
	signalDaemonStopForEnsure = func(rec kitdaemon.RuntimeRecord, _ string) error {
		return os.Remove(filepath.Join(ns.DataDir, fmt.Sprintf("daemon.%d.json", rec.PID)))
	}
	t.Cleanup(func() { signalDaemonStopForEnsure = origSignal })

	err = stopRunningDaemons(context.Background(), ns.DataDir, ns.DBHash)

	require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	assert.Contains(t, err.Error(), unreachableAddress)
}

func TestStopRunningDaemonsReturnsSignalError(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
		"pid":     os.Getpid(),
	})
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), addr))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)

	origSignal := signalDaemonStopForEnsure
	signalDaemonStopForEnsure = func(kitdaemon.RuntimeRecord, string) error {
		return assert.AnError
	}
	t.Cleanup(func() { signalDaemonStopForEnsure = origSignal })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = stopRunningDaemons(ctx, ns.DataDir, ns.DBHash)
	require.ErrorIs(t, err, assert.AnError)
}

func TestStopRunningDaemonsErrorsOnUnverifiableIncompatibleRuntime(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
	})
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), addr))
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err = stopRunningDaemons(ctx, ns.DataDir, ns.DBHash)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PID could not be verified")

	_, err = os.Stat(filepath.Join(ns.DataDir, fmt.Sprintf("daemon.%d.json", os.Getpid())))
	assert.NoError(t, err, "unverifiable reachable daemon runtime file should be preserved")
}

func TestEnsureRunningReportsLiveUnreachableDaemonWithoutAutoStart(t *testing.T) {
	tmp := setupKataEnv(t)
	address := "unix://" + filepath.Join(tmp, "missing.sock")
	require.NoError(t, writeRuntimeRecord(t, tmp, address))
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://new-daemon")

	_, err := EnsureRunning(context.Background())

	require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	assert.Contains(t, err.Error(), fmt.Sprintf("daemon pid %d is running", os.Getpid()))
	assert.Contains(t, err.Error(), address)
	assert.Contains(t, err.Error(), "missing.sock")
	assert.Zero(t, restore.startCalls)
}

func TestEnsureRunningPrioritizesUnreachableDaemonOverIncompatibleDaemon(t *testing.T) {
	t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
	tmp := setupKataEnv(t)
	child, _ := startLongLivedTestProcess(t)
	_, addr := startMockDaemonPing(t, map[string]any{
		"ok":      true,
		"service": "kata",
		"version": "old-version",
		"pid":     child.Process.Pid,
	})
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, child.Process.Pid, addr))
	unreachableAddress := "unix://" + filepath.Join(tmp, "missing.sock")
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), unreachableAddress))
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://new-daemon")

	_, err := EnsureRunning(context.Background())

	require.ErrorIs(t, err, ErrLocalDaemonUnreachable)
	assert.Contains(t, err.Error(), unreachableAddress)
	assert.Zero(t, restore.stopCalls)
	assert.Zero(t, restore.startCalls)
}

func TestDiscoverIgnoresRuntimeRecordWhosePIDWasReused(t *testing.T) {
	tmp := setupKataEnv(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentity:   identity,
		ProcessIdentityV2: mismatchedProcessIdentity(t, identity),
		Address:           "unix://" + filepath.Join(tmp, "missing.sock"),
		StartedAt:         time.Now().UTC(),
	})
	require.NoError(t, err)

	_, ok, err = Discover(context.Background(), ns.DataDir)

	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDiscoverStopsProbingAfterFirstLiveDaemon(t *testing.T) {
	tmp := setupKataEnv(t)

	var firstHits, secondHits atomic.Int32
	_, addr1 := startCountingPing(t, &firstHits)
	_, addr2 := startCountingPing(t, &secondHits)

	// Two distinct alive PIDs: this process and a long-lived helper.
	helper, _ := startLongLivedTestProcess(t)
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, os.Getpid(), addr1))
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, helper.Process.Pid, addr2))

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)

	url, ok, err := Discover(context.Background(), ns.DataDir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, []string{"http://" + addr1, "http://" + addr2}, url)

	total := firstHits.Load() + secondHits.Load()
	assert.Equal(t, int32(1), total,
		"Discover must stop probing after the first live record; an eager scan probes both")
}

// startCountingPing is startMockDaemonPing with a hit counter, for asserting
// that a scan short-circuits rather than probing every record.
func startCountingPing(t *testing.T, hits *atomic.Int32) (url, addr string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "service": "kata", "version": "test-version", "pid": os.Getpid(),
		})
	}))
	t.Cleanup(server.Close)
	return server.URL, strings.TrimPrefix(server.URL, "http://")
}

func mismatchedProcessIdentity(t *testing.T, identity kitdaemon.ProcessIdentity) kitdaemon.ProcessIdentity {
	t.Helper()
	encoded := string(identity)
	last := encoded[len(encoded)-1]
	replacement := byte('0')
	if last == replacement {
		replacement = '1'
	}
	return kitdaemon.ProcessIdentity(encoded[:len(encoded)-1] + string(replacement))
}

func TestEnsureRunningAutoStartsWhenRuntimePIDWasReused(t *testing.T) {
	tmp := setupKataEnv(t)
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentity:   identity,
		ProcessIdentityV2: mismatchedProcessIdentity(t, identity),
		Address:           "unix://" + filepath.Join(tmp, "missing.sock"),
		StartedAt:         time.Now().UTC(),
	})
	require.NoError(t, err)
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://new-daemon")

	url, err := EnsureRunning(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "http://new-daemon", url)
	assert.Equal(t, 1, restore.startCalls)
}

func TestEnsureRunningAutoStartsWhenRuntimePIDIsDead(t *testing.T) {
	tmp := setupKataEnv(t)
	address := "unix://" + filepath.Join(tmp, "missing.sock")
	require.NoError(t, writeRuntimeRecordForPID(t, tmp, 1<<30, address))
	restore := patchEnsureHooks(t, currentVersionForEnsure(), "http://new-daemon")

	url, err := EnsureRunning(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "http://new-daemon", url)
	assert.Equal(t, 1, restore.startCalls)
}

// setupKataEnv points KATA_HOME and KATA_DB at a fresh temp dir so the test
// runs in isolation from any developer-local state. Returns the temp dir.
func setupKataEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	return tmp
}

// startMockDaemonPing starts an httptest.Server that responds to
// /api/v1/ping with the given JSON payload and 404s every other path.
// Returns the full URL and the host:port address used in runtime records.
func startMockDaemonPing(t *testing.T, payload map[string]any) (url, addr string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ping" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server.URL, strings.TrimPrefix(server.URL, "http://")
}

func compatibleEnsureDiscovery(address string) ensureDiscovery {
	return ensureDiscovery{
		Outcome: daemonScanCompatible,
		Daemon: liveDaemon{
			BaseURL: "http://" + address,
			Record:  kitdaemon.RuntimeRecord{Address: address},
		},
	}
}

func writeRuntimeRecord(t *testing.T, home, addr string) error {
	t.Helper()
	return writeRuntimeRecordForPID(t, home, os.Getpid(), addr)
}

func writeRuntimeRecordForPID(t *testing.T, home string, pid int, addr string) error {
	t.Helper()
	ns, err := daemon.NewNamespace()
	if err != nil {
		return err
	}
	if err := ns.EnsureDirs(); err != nil {
		return err
	}
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID:       pid,
		Address:   addr,
		Metadata:  map[string]string{"db_path": filepath.Join(home, "kata.db")},
		StartedAt: time.Now().UTC(),
	})
	return err
}

type ensurePatchState struct {
	stopCalls  int
	startCalls int
}

func patchEnsureHooks(t *testing.T, version, startedURL string) *ensurePatchState {
	t.Helper()
	state := &ensurePatchState{}
	origCurrent := currentVersionForEnsure
	origStop := stopRunningDaemonsForEnsure
	origStart := startDaemonForEnsure
	currentVersionForEnsure = func() string { return version }
	stopRunningDaemonsForEnsure = func(context.Context, string, string) error {
		state.stopCalls++
		return nil
	}
	startDaemonForEnsure = func(context.Context, string) (RunningDaemon, error) {
		state.startCalls++
		return remoteRunningDaemon(startedURL, false), nil
	}
	t.Cleanup(func() {
		currentVersionForEnsure = origCurrent
		stopRunningDaemonsForEnsure = origStop
		startDaemonForEnsure = origStart
	})
	return state
}
