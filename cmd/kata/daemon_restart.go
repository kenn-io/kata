package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"sync/atomic"

	"go.kenn.io/kata/internal/daemonruntime"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kit/pathresolve"
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

// resolveDaemonExecutable is swapped in tests that exercise unsupported hosts.
var resolveDaemonExecutable = os.Executable

// newDaemonRestart resolves the executable to re-execute before any update
// can replace it: Linux reports a replaced binary as deleted. A host that
// cannot resolve the running binary still starts the daemon; it only loses
// automatic restart, and stderr says so. Ephemeral test and go-build binaries
// are silently excluded.
func newDaemonRestart(stderr io.Writer) *daemonRestart {
	executable, err := resolveDaemonExecutable()
	if err == nil {
		executable, err = pathresolve.EvalSymlinks(executable)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kata daemon: automatic restart after updates disabled: %v\n", err)
		return nil
	}
	if kitdaemon.IsEphemeralExecutable(executable) {
		return nil
	}
	return &daemonRestart{executable: executable}
}

// watch validates each restart request before triggering daemon shutdown.
func (r *daemonRestart) watch(ctx context.Context, sigs <-chan os.Signal, validate func() error, logger *log.Logger, trigger func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			if err := validate(); err != nil {
				logger.Printf("restart: validate replacement: %v", err)
				continue
			}
			r.requested.Store(true)
			trigger()
			return
		}
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

// daemonReplacementWaitTimeout is overridable for refused-restart tests.
var daemonReplacementWaitTimeout = daemonRestartProcessWaitTimeout

func requestDaemonRestart(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord) (kitdaemon.RuntimeRecord, error) {
	if err := daemonruntime.SignalDaemonRestart(previous, ns.DBHash); err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("signal daemon pid %d: %w", previous.PID, err)
	}
	replacement, err := waitForDaemonReplacement(ctx, kitdaemon.RuntimeStore{Dir: ns.DataDir}, previous)
	if err != nil {
		if tail := daemonStartLogTail(ns.DataDir); tail != "" {
			err = fmt.Errorf("%w\n%s", err, tail)
		}
	}
	return replacement, err
}
