package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/version"
)

// BaseURLKey is the context key for injecting a daemon base URL during
// tests, bypassing both Discover and the auto-start path. CLI and TUI
// callers honor it via EnsureRunning.
type BaseURLKey struct{}

const (
	daemonServiceName       = "kata"
	skipDaemonVersionEnvVar = "KATA_SKIP_DAEMON_VERSION_CHECK"
)

var (
	currentVersionForEnsure     = func() string { return version.Version }
	startDaemonForEnsure        = autoStart
	stopRunningDaemonsForEnsure = stopRunningDaemons
)

// EnsureRunning returns a live daemon's base URL, auto-starting the daemon
// if no live record is found. Callers that should never spawn a daemon
// (health probes, list commands that should fail loudly) should call
// Discover directly instead.
//
// Test callers can short-circuit discovery by stashing a base URL on ctx
// under BaseURLKey{}.
//
// .kata.local.toml discovery walks upward from CWD. Commands that
// target a specific workspace via --workspace should call
// EnsureRunningInWorkspace instead so the walk anchors to the
// targeted workspace.
func EnsureRunning(ctx context.Context) (string, error) {
	return EnsureRunningInWorkspace(ctx, "")
}

// EnsureRunningInWorkspace is the workspace-aware variant of
// EnsureRunning. workspaceStart is the absolute path to begin the
// .kata.local.toml walk from; pass "" to fall back to CWD. Empty is
// the right value when no --workspace flag is in play; non-empty is
// required so that running `kata --workspace /repo create ...` from
// outside the repo still picks up /repo/.kata.local.toml's [server]
// override instead of falling through to local auto-start.
func EnsureRunningInWorkspace(ctx context.Context, workspaceStart string) (string, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return v, nil
	}
	if url, ok, err := resolveRemote(ctx, workspaceStart); err != nil {
		return "", err
	} else if ok {
		return url, nil
	}
	return ensureLocalRunning(ctx)
}

// EnsureLocalRunning returns a live local daemon's base URL, ignoring
// KATA_SERVER and .kata.local.toml remote overrides. Named "local" TUI
// daemon entries use this so selecting local never silently resolves to
// a configured shared daemon.
func EnsureLocalRunning(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(BaseURLKey{}).(string); ok && v != "" {
		return v, nil
	}
	return ensureLocalRunning(ctx)
}

func ensureLocalRunning(ctx context.Context) (string, error) {
	ns, err := daemon.NewNamespace()
	if err != nil {
		return "", err
	}
	if url, compatible, ok := discoverForEnsure(ctx, ns.DataDir); ok {
		if compatible {
			return url, nil
		}
		if err := stopRunningDaemonsForEnsure(ctx, ns.DataDir); err != nil {
			return "", err
		}
		return startDaemonForEnsure(ctx, ns.DataDir)
	}
	return startDaemonForEnsure(ctx, ns.DataDir)
}

func discoverForEnsure(ctx context.Context, dataDir string) (string, bool, bool) {
	recs, err := daemon.ListRuntimeFiles(dataDir)
	if err != nil {
		return "", false, false
	}
	var staleURL string
	for _, r := range recs {
		if !daemon.ProcessAlive(r.PID) {
			continue
		}
		url, info, ok := probeAddress(ctx, r.Address)
		if !ok {
			continue
		}
		if daemonVersionCheckSkipped() || daemonVersionCompatible(info) {
			return url, true, true
		}
		if staleURL == "" {
			staleURL = url
		}
	}
	if staleURL != "" {
		return staleURL, false, true
	}
	return "", false, false
}

func daemonVersionCheckSkipped() bool {
	return os.Getenv(skipDaemonVersionEnvVar) == "1"
}

func daemonVersionCompatible(info PingInfo) bool {
	return info.Service == daemonServiceName && info.Version == currentVersionForEnsure()
}

func stopRunningDaemons(ctx context.Context, dataDir string) error {
	recs, err := daemon.ListRuntimeFiles(dataDir)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if !daemon.ProcessAlive(r.PID) {
			continue
		}
		_, info, ok := probeAddress(ctx, r.Address)
		if !ok || info.Service != daemonServiceName || daemonVersionCompatible(info) {
			continue
		}
		if info.PID == 0 || info.PID != r.PID {
			return fmt.Errorf("daemon at %s is running but its PID could not be verified; stop it manually", r.Address)
		}
		p, err := os.FindProcess(r.PID)
		if err != nil {
			continue
		}
		_ = p.Signal(syscall.SIGTERM)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := discoverForEnsure(ctx, dataDir); !ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("old daemon did not stop within 3s")
}

func autoStart(ctx context.Context, dataDir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if shouldRefuseAutoStartDaemon(exe) {
		return "", fmt.Errorf("refusing to auto-start daemon from ephemeral binary %s", filepath.Base(exe))
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
	// Mark the child as an implicit auto-start so it skips the PORT-env
	// listen path (see listenFromPortEnv). The child inherits the
	// parent's environment, so a stray PORT in a developer's shell would
	// otherwise flip every implicit daemon onto wildcard TCP.
	cmd.Env = append(os.Environ(), daemon.AutoStartMarkerEnv+"=1")
	// Detach the child into its own process group so SIGINT delivered to the
	// foreground caller (e.g. ctrl-C on `kata create` or `kata tui`) is not
	// propagated to the daemon we just spawned.
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("auto-start daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if url, compatible, ok := discoverForEnsure(ctx, dataDir); ok && compatible {
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
	//nolint:gosec // G304: dataDir is the daemon's own data dir; filename is the fixed constant "daemon.log".
	f, err := os.OpenFile(filepath.Join(dataDir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func shouldRefuseAutoStartDaemon(exe string) bool {
	base := filepath.Base(exe)
	// Normalize separators: on Windows the go-build temp dir is "\go-build…",
	// but callers (and tests) may pass forward-slash paths. ToSlash makes the
	// check work regardless of how the path was formed.
	return strings.HasSuffix(base, ".test") || strings.Contains(filepath.ToSlash(exe), "/go-build")
}
