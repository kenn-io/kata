package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

func TestShouldRefuseAutoStartDaemonFromGoTestBinary(t *testing.T) {
	assert.True(t, shouldRefuseAutoStartDaemon("/tmp/go-build123/b001/kata.test"))
	assert.True(t, shouldRefuseAutoStartDaemon("/var/folders/x/go-build123/b001/kata"))
	assert.False(t, shouldRefuseAutoStartDaemon("/usr/local/bin/kata"))
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
	startDaemonForEnsure = func(context.Context, string) (string, error) {
		state.startCalls++
		return startedURL, nil
	}
	t.Cleanup(func() {
		currentVersionForEnsure = origCurrent
		stopRunningDaemonsForEnsure = origStop
		startDaemonForEnsure = origStart
	})
	return state
}
