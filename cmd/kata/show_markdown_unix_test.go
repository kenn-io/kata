//go:build !windows

package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExternalShowMarkdownRendererTimeoutKillsDescendant(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
	renderer.timeout = 100 * time.Millisecond
	renderer.grace = 50 * time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		_, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
		errCh <- err
	}()

	pid := waitForHelperPID(t, renderer.argv[len(renderer.argv)-1])
	err := <-errCh
	require.ErrorIs(t, err, context.DeadlineExceeded)
	requireProcessGone(t, pid)
}

func TestExternalShowMarkdownRendererCancellationKillsDescendant(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
	renderer.grace = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		_, err := renderer.Render(ctx, markdownDescription, "body", 80)
		errCh <- err
	}()

	pid := waitForHelperPID(t, renderer.argv[len(renderer.argv)-1])
	cancel()
	err := <-errCh
	require.ErrorIs(t, err, context.Canceled)
	requireProcessGone(t, pid)
}

func TestExternalShowMarkdownRendererBoundsInheritedDescendantStdout(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER_DETACH", "1")
	renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
	renderer.timeout = 100 * time.Millisecond
	renderer.grace = 50 * time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		_, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
		errCh <- err
	}()

	pid := waitForHelperPID(t, renderer.argv[len(renderer.argv)-1])
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})

	started := time.Now()
	err := <-errCh
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}

func configureShowMarkdownHelperChild(child *exec.Cmd) {
	if os.Getenv("GO_WANT_SHOW_MARKDOWN_HELPER_DETACH") == "1" {
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
}

func waitForHelperPID(t *testing.T, readyPath string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(readyPath)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			require.NoError(t, err)
			return pid
		}
		require.ErrorIs(t, err, fs.ErrNotExist)
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("renderer helper did not signal readiness")
	return 0
}

func requireProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("renderer descendant %d survived cancellation", pid)
}
