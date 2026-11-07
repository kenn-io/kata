package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func stubDaemonExecutable(t *testing.T, resolve func() (string, error)) {
	t.Helper()
	original := resolveDaemonExecutable
	resolveDaemonExecutable = resolve
	t.Cleanup(func() { resolveDaemonExecutable = original })
}

func TestNewDaemonRestart_UnresolvableExecutableDisablesRestart(t *testing.T) {
	stubDaemonExecutable(t, func() (string, error) { return "", errors.New("procfs unavailable") })
	var stderr bytes.Buffer
	restart := newDaemonRestart(&stderr)
	assert.Nil(t, restart, "startup must continue without automatic restart")
	assert.Contains(t, stderr.String(), "procfs unavailable")
	assert.Contains(t, stderr.String(), "automatic restart")
}

func TestNewDaemonRestart_MissingExecutableDisablesRestart(t *testing.T) {
	stubDaemonExecutable(t, func() (string, error) { return filepath.Join(t.TempDir(), "kata"), nil })
	var stderr bytes.Buffer
	assert.Nil(t, newDaemonRestart(&stderr))
	assert.Contains(t, stderr.String(), "automatic restart")
}

func TestNewDaemonRestart_EphemeralExecutableIsSilent(t *testing.T) {
	stubDaemonExecutable(t, os.Executable)
	var stderr bytes.Buffer
	assert.Nil(t, newDaemonRestart(&stderr), "test binaries never re-execute")
	assert.Empty(t, stderr.String())
}

func TestNewDaemonRestart_ResolvesSymlinkedExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kata")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0o600))
	link := filepath.Join(dir, "kata-link")
	require.NoError(t, os.Symlink(target, link))
	stubDaemonExecutable(t, func() (string, error) { return link, nil })
	var stderr bytes.Buffer
	restart := newDaemonRestart(&stderr)
	require.NotNil(t, restart)
	// Windows temp dirs may carry 8.3 short names; resolve the expected path the same way.
	expected, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Equal(t, expected, restart.executable)
	assert.Empty(t, stderr.String())
}

func TestDaemonRestart_SignalFailureCleansRequest(t *testing.T) {
	dir := t.TempDir()
	rec := kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: time.Now().UTC(), Metadata: map[string]string{daemonruntime.RuntimeRestartExecutableKey: "1"}}
	_, err := requestDaemonRestart(t.Context(), &daemonruntime.Namespace{DataDir: dir}, rec, "/installed/kata", nil)
	require.ErrorContains(t, err, "signal daemon pid")
	require.NoFileExists(t, restartRequestPath(dir, rec))
	replies, err := filepath.Glob(filepath.Join(dir, "restart-reply-*"))
	require.NoError(t, err)
	assert.Empty(t, replies)
}

func TestDaemonRestartAccept_RefusesUnavailableReplacementAndReplies(t *testing.T) {
	dir := t.TempDir()
	rec := kitdaemon.RuntimeRecord{PID: os.Getpid(), StartedAt: time.Now()}
	req := restartRequest{Executable: filepath.Join(dir, "missing-kata"), Reply: "restart-reply"}
	payload, err := json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(restartRequestPath(dir, rec), payload, 0o600))
	reply := filepath.Join(dir, req.Reply)
	require.NoError(t, os.WriteFile(reply, nil, 0o600))
	restart := &daemonRestart{executable: "original-kata"}

	err = restart.accept(t.Context(), dir, rec)

	require.ErrorContains(t, err, "check replacement startup")
	assert.Equal(t, "original-kata", restart.executable)
	failure, err := os.ReadFile(reply) //nolint:gosec // test-owned reply under t.TempDir
	require.NoError(t, err)
	assert.Contains(t, string(failure), "check replacement startup")
}

// A newer client may send fields this daemon does not know. The daemon must
// still act on the fields it does know instead of refusing the request.
func TestDaemonRestartAccept_IgnoresFieldsFromNewerClients(t *testing.T) {
	dir := t.TempDir()
	rec := kitdaemon.RuntimeRecord{PID: os.Getpid(), StartedAt: time.Now()}
	executable := filepath.Join(dir, "missing-kata")
	payload, err := json.Marshal(map[string]any{
		"Executable": executable,
		"Reply":      "restart-reply",
		"Flags":      []string{},
		"FutureKey":  "set by a newer client",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(restartRequestPath(dir, rec), payload, 0o600))
	reply := filepath.Join(dir, "restart-reply")
	require.NoError(t, os.WriteFile(reply, nil, 0o600))
	restart := &daemonRestart{executable: "original-kata"}

	err = restart.accept(t.Context(), dir, rec)

	require.ErrorContains(t, err, "check replacement startup")
	// The exec error quotes the path, which doubles Windows separators, so
	// match the requested file name rather than the full path.
	require.ErrorContains(t, err, filepath.Base(executable))
	failure, err := os.ReadFile(reply) //nolint:gosec // test-owned reply under t.TempDir
	require.NoError(t, err)
	assert.Contains(t, string(failure), "check replacement startup")
}

func TestDaemonArgs(t *testing.T) {
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"kata", "daemon", "start", "--foreground", "--listen", "127.0.0.1:7777", "--insecure-readonly", "--no-auto-token=false", "--"}
	overrides := []string{"--listen=127.0.0.1:8888", "--insecure-readonly=false", "--no-auto-token=true"}
	want := []string{"daemon", "start", "--foreground", "--listen=127.0.0.1:8888", "--insecure-readonly=false", "--no-auto-token=true", "--"}
	args := daemonArgs(overrides...)
	assert.Equal(t, want, args)
	os.Args = append([]string{"kata"}, args...)
	assert.Equal(t, want, daemonArgs(overrides...))
}

func TestDaemonRestartExec_ReportsExecFailureToWaitingCaller(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_DSN", "")
	ns, err := daemonruntime.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(ns.DataDir, 0o700))
	reply := filepath.Join(ns.DataDir, "restart-reply-caller")
	require.NoError(t, os.WriteFile(reply, nil, 0o600))
	identity, ok := kitdaemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	restart := &daemonRestart{executable: filepath.Join(t.TempDir(), "missing-kata"), reply: filepath.Base(reply)}
	restart.requested.Store(true)

	synctest.Test(t, func(t *testing.T) {
		previous := kitdaemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, StartedAt: time.Now()}
		result := make(chan error, 1)
		go func() {
			_, err := waitForDaemonReplacement(t.Context(), kitdaemon.RuntimeStore{Dir: ns.DataDir}, previous, reply)
			result <- err
		}()
		synctest.Wait()

		execErr := restart.exec(t.Context())

		require.ErrorContains(t, execErr, "restart daemon")
		require.ErrorContains(t, <-result, errors.Unwrap(execErr).Error(), "the waiting caller must see the exec error")
	})
}

func TestDaemonRestartAccept_WaitsForSlowReplacementCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the replacement executable")
	}
	dir := t.TempDir()
	// A freshly installed binary can take several seconds to start, for
	// example while a scanner inspects it.
	slow := filepath.Join(dir, "slow-kata")
	require.NoError(t, os.WriteFile(slow, []byte("#!/bin/sh\nsleep 6\n"), 0o700)) //nolint:gosec // G306: test-owned executable script
	rec := kitdaemon.RuntimeRecord{PID: os.Getpid(), StartedAt: time.Now()}
	payload, err := json.Marshal(restartRequest{Executable: slow, Reply: "restart-reply"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(restartRequestPath(dir, rec), payload, 0o600))
	reply := filepath.Join(dir, "restart-reply")
	require.NoError(t, os.WriteFile(reply, nil, 0o600))
	restart := &daemonRestart{executable: "original-kata"}

	require.NoError(t, restart.accept(t.Context(), dir, rec))

	assert.Equal(t, slow, restart.executable)
	failure, err := os.ReadFile(reply) //nolint:gosec // test-owned reply under t.TempDir
	require.NoError(t, err)
	assert.Empty(t, failure)
}

func TestRestartLocalDaemon_UnresolvableExecutableKeepsCallerStderrQuiet(t *testing.T) {
	stubDaemonExecutable(t, func() (string, error) { return "", errors.New("procfs unavailable") })
	read, write, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stderr
	os.Stderr = write
	t.Cleanup(func() { os.Stderr = original })

	_, restartErr := client.RestartLocalDaemon(t.Context(), &daemonruntime.Namespace{DataDir: t.TempDir()}, kitdaemon.RuntimeRecord{PID: 1 << 30})

	os.Stderr = original
	require.NoError(t, write.Close())
	stderr, err := io.ReadAll(read)
	require.NoError(t, err)
	require.ErrorContains(t, restartErr, "cannot resolve installed executable")
	assert.Empty(t, string(stderr), "a CLI command must not print the daemon's startup diagnostic")
}

func TestDaemonRestart_RemovesAbandonedRestartFiles(t *testing.T) {
	dir := t.TempDir()
	abandoned := []string{"restart-reply-abandoned", "restart-reply-abandoned.pid", "restart.4242.1700000000000000000"}
	for _, name := range abandoned {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	log := filepath.Join(dir, "daemon.log")
	require.NoError(t, os.WriteFile(log, nil, 0o600))
	rec := kitdaemon.RuntimeRecord{PID: 1 << 30, StartedAt: time.Now().UTC(), Metadata: map[string]string{daemonruntime.RuntimeRestartExecutableKey: "1"}}

	_, err := requestDaemonRestart(t.Context(), &daemonruntime.Namespace{DataDir: dir}, rec, "/installed/kata", nil)

	require.ErrorContains(t, err, "signal daemon pid")
	for _, name := range abandoned {
		assert.NoFileExists(t, filepath.Join(dir, name), "the caller holds the start lock, so earlier restart files are abandoned")
	}
	assert.FileExists(t, log)
}

func TestDaemonRestartWatch_IdleDeadlineDuringValidationKeepsRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		idleExpired := make(chan struct{})
		idle := daemon.NewIdleController(time.Second, func() { close(idleExpired) })
		idle.Start()
		sigs := make(chan os.Signal, 1)
		sigs <- os.Interrupt
		triggered := false
		validate := func() error {
			time.Sleep(5 * time.Second)
			select {
			case <-idleExpired:
				return errors.New("idle shutdown canceled the replacement check")
			default:
				return nil
			}
		}
		restart := &daemonRestart{}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			// Daemon shutdown stops idle admission, as the trigger does here.
			restart.watch(ctx, sigs, idle, validate, log.New(io.Discard, "", 0), func() {
				triggered = true
				idle.Stop()
			})
		}()
		time.Sleep(10 * time.Second)
		cancel()
		<-done

		assert.True(t, triggered, "accepted restart must trigger shutdown")
		assert.True(t, restart.requested.Load(), "accepted restart must be recorded")
		select {
		case <-idleExpired:
			t.Fatal("idle shutdown fired while the restart was being validated")
		default:
		}
	})
}

func TestDaemonRestartWatch_RefusalReleasesIdleHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		idleExpired := make(chan struct{})
		idle := daemon.NewIdleController(time.Second, func() { close(idleExpired) })
		idle.Start()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sigs := make(chan os.Signal, 1)
		sigs <- os.Interrupt
		done := make(chan struct{})
		go func() {
			defer close(done)
			(&daemonRestart{}).watch(ctx, sigs, idle, func() error { return errors.New("refused") }, log.New(io.Discard, "", 0), func() {})
		}()

		time.Sleep(2 * time.Second)
		select {
		case <-idleExpired:
		default:
			t.Fatal("a refused restart must not keep the daemon from idling")
		}
		cancel()
		<-done
	})
}

func TestDaemonRestartKeepsBrowserAddress(t *testing.T) {
	for _, transport := range []string{"127.0.0.1:0", ""} {
		t.Run(transport, func(t *testing.T) {
			setupKataEnv(t)
			t.Setenv("PORT", "")
			t.Setenv("KATA_TELEMETRY_DISABLED", "1")
			original := startedForRestartWebListen
			t.Cleanup(func() { startedForRestartWebListen = original })
			startedForRestartWebListen = ""
			start := func() (string, func()) {
				ctx, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				done := make(chan error, 1)
				go func() { done <- runDaemonProcess(ctx, transport, false, true, nil) }()
				ns, err := daemonruntime.NewNamespace()
				require.NoError(t, err)
				var origin string
				require.Eventually(t, func() bool { //nolint:kennlint // sleeptest: waits for a real daemon to publish its runtime file
					records, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
					if err != nil || len(records) != 1 {
						return false
					}
					origin = records[0].Metadata["web_origin"]
					return origin != ""
				}, 5*time.Second, 10*time.Millisecond)
				stop := func() {
					cancel()
					select {
					case err := <-done:
						require.NoError(t, err)
					case <-time.After(5 * time.Second):
						t.Fatal("daemon did not stop")
					}
				}
				return origin, stop
			}
			origin, stop := start()
			stop()
			startedForRestartWebListen = strings.TrimPrefix(origin, "http://")
			replacement, stop := start()
			require.Equal(t, origin, replacement)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, origin+"/api/v1/ping", nil)
			require.NoError(t, err)
			response, err := (&http.Client{Timeout: time.Second}).Do(req)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			require.Equal(t, 200, response.StatusCode)
			stop()
			occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", strings.TrimPrefix(origin, "http://"))
			require.NoError(t, err)
			defer func() { _ = occupied.Close() }()
			fallback, stop := start()
			defer stop()
			require.NotEqual(t, origin, fallback, "occupied previous ports must fall back to a fresh address")
		})
	}
}

func TestDaemonRestartAddress(t *testing.T) {
	for _, tc := range []struct{ name, listen, origin, requested, want string }{
		{"default", "", "", "", "127.0.0.1:54321"},
		{"shared", "", "", "127.0.0.1:0", "127.0.0.1:54321"},
		{"zero", "127.0.0.1:0", "", "", "127.0.0.1:54321"},
		{"pinned web", "127.0.0.1:12345", "", "127.0.0.1:0", ""},
		{"pinned main", "", "", "127.0.0.1:12345", ""},
		{"public", "127.0.0.1:0", "https://daemon.example", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, restartAddress(config.WebConfig{Listen: tc.listen, PublicOrigin: tc.origin}, tc.requested, "127.0.0.1:54321"))
		})
	}
}
