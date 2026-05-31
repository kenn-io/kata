package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/wesm/kata/internal/daemon"
)

// BaseURLKey is the context key for injecting a daemon base URL during
// tests, bypassing both Discover and the auto-start path. CLI and TUI
// callers honor it via EnsureRunning.
type BaseURLKey struct{}

// EnsureRunning returns a live daemon's base URL, auto-starting the daemon
// if no live record is found. Callers that should never spawn a daemon
// (health probes, list commands that should fail loudly) should call
// Discover directly instead.
//
// Test callers can short-circuit discovery by stashing a base URL on ctx
// under BaseURLKey{}.
func EnsureRunning(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return v, nil
	}
	ns, err := daemon.NewNamespace()
	if err != nil {
		return "", err
	}
	if url, ok := Discover(ctx, ns.DataDir); ok {
		return url, nil
	}
	return autoStart(ctx, ns.DataDir)
}

func autoStart(ctx context.Context, dataDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	//nolint:gosec // G204: exe is os.Executable()
	cmd := exec.Command(exe, "daemon", "start")
	// The auto-started daemon outlives this process, so it must not inherit
	// our stdio. Inheriting the caller's stderr keeps that handle open after
	// the daemon detaches, which hangs any parent that captures our output
	// (command substitution, CI, pipelines). Send the daemon's stdout/stderr
	// to a daemon.log file under the data dir; if that can't be opened, leave
	// them nil so exec connects the child to the null device. Either way we
	// never hand the daemon the caller's stderr.
	if logw := daemonLogWriter(dataDir); logw != nil {
		cmd.Stdout = logw
		cmd.Stderr = logw
		defer func() { _ = logw.Close() }() // child keeps its own handle after Start
	}
	// Detach the child into its own process group (and, on Windows, off the
	// caller's console) so SIGINT delivered to the foreground caller (e.g.
	// ctrl-C on `kata create` or `kata tui`) is not propagated to the daemon,
	// and the daemon never depends on the caller's console lifetime.
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("auto-start daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if url, ok := Discover(ctx, dataDir); ok {
			return url, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return "", errors.New("daemon failed to start within 5s")
}

// daemonLogWriter opens <dataDir>/daemon.log for the auto-started daemon's
// stdout+stderr. Returns nil (so exec falls back to the null device) if the
// directory or file cannot be created — the caller must never substitute its
// own stderr, which a detached daemon would hold open and hang the caller.
func daemonLogWriter(dataDir string) *os.File {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
