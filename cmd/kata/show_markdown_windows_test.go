//go:build windows

package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExternalShowMarkdownRendererTimeoutBoundsInheritedDescendantStdout(t *testing.T) {
	t.Setenv("GO_WANT_SHOW_MARKDOWN_HELPER", "1")
	renderer := helperRenderer("spawn-descendant", filepath.Join(t.TempDir(), "ready"))
	renderer.timeout = 100 * time.Millisecond
	renderer.grace = 50 * time.Millisecond

	errCh := make(chan error, 1)
	go func() {
		_, err := renderer.Render(context.Background(), markdownDescription, "body", 80)
		errCh <- err
	}()

	pid := waitForWindowsHelperPID(t, renderer.argv[len(renderer.argv)-1])
	t.Cleanup(func() { killWindowsHelper(pid) })

	started := time.Now()
	err := <-errCh
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}

func TestExternalShowMarkdownRendererCancellationBoundsInheritedDescendantStdout(t *testing.T) {
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

	pid := waitForWindowsHelperPID(t, renderer.argv[len(renderer.argv)-1])
	t.Cleanup(func() { killWindowsHelper(pid) })

	started := time.Now()
	cancel()
	err := <-errCh
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), time.Second)
}

func configureShowMarkdownHelperChild(_ *exec.Cmd) {}

func waitForWindowsHelperPID(t *testing.T, readyPath string) int {
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

func killWindowsHelper(pid int) {
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Kill()
		_ = process.Release()
	}
}
