package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	kitdaemon "go.kenn.io/kit/daemon"
)

// daemonRestart carries a restart request from the platform signal source to
// the point where the daemon has released its listeners and runtime record.
// The daemon re-executes itself with its own arguments and environment, so
// the replacement keeps the listener, read-only mode, credentials, auto-start
// marker, and idle timeout of the process it replaces.
type daemonRestart struct {
	executable string
	requested  atomic.Bool
}

// newDaemonRestart resolves the executable to re-execute before any update
// can replace it: Linux reports a replaced binary as deleted. Ephemeral test
// and go-build binaries return nil, which disables the restart signal.
func newDaemonRestart() (*daemonRestart, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve daemon executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve daemon executable: %w", err)
	}
	if kitdaemon.IsEphemeralExecutable(executable) {
		return nil, nil
	}
	return &daemonRestart{executable: executable}, nil
}

// watch records the first restart signal and triggers daemon shutdown.
func (r *daemonRestart) watch(ctx context.Context, sigs <-chan os.Signal, trigger func()) {
	select {
	case <-ctx.Done():
	case <-sigs:
		r.requested.Store(true)
		trigger()
	}
}

// exec replaces the daemon once the current process has shut down. On Unix
// the process image is replaced in place so supervisors keep the same PID; on
// Windows a detached replacement starts and this process exits.
func (r *daemonRestart) exec(ctx context.Context) error {
	if r == nil || !r.requested.Load() {
		return nil
	}
	if err := reexecDaemon(ctx, r.executable); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	return nil
}
