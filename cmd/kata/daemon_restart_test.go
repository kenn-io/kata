package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
