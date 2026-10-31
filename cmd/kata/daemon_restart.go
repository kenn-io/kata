package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"go.kenn.io/kata/internal/daemonruntime"

	"go.kenn.io/kit/atomicfile"
	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"

	"go.kenn.io/kit/pathresolve"
)

// daemonRestart carries a restart request from the platform signal source to
// the point where the daemon has released its listeners and runtime record.
// The daemon re-executes itself with its own arguments and environment, so
// the replacement keeps the listener, read-only mode, credentials, auto-start
// marker, and idle timeout of the process it replaces.
type daemonRestart struct {
	executable string
	args       []string
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
	if err := reexecDaemon(ctx, r.executable, r.args); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	return nil
}

func daemonArgs(extra ...string) []string {
	args := slices.Clone(os.Args[1:])
	if at := slices.Index(args, "--"); at >= 0 {
		return slices.Insert(args, at, extra...)
	}
	return append(args, extra...)
}

func validateDaemonReplacement(ctx context.Context, executable string, flags []string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, daemonArgs(append(flags, "--check-startup")...)...) //nolint:gosec // selected installed executable with daemon-owned arguments
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("check replacement startup: %w: %s", err, output)
	}
	return nil
}

type restartRequest struct {
	Executable, Reply string
	Flags             []string
}

func restartRequestPath(dir string, rec kitdaemon.RuntimeRecord) string {
	return filepath.Join(dir, fmt.Sprintf("restart.%d.%d", rec.PID, rec.StartedAt.UnixNano()))
}

func requestDaemonRestart(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord, executable string, flags []string) (kitdaemon.RuntimeRecord, error) {
	var reply string
	if previous.Metadata[daemonruntime.RuntimeRestartExecutableKey] == "1" {
		file, err := safefileio.CreatePrivateTemp(ns.DataDir, "restart-reply-*")
		if err != nil {
			return kitdaemon.RuntimeRecord{}, err
		}
		reply = file.Name()
		defer func() { _ = os.Remove(reply) }()
		if err := file.Close(); err != nil {
			return kitdaemon.RuntimeRecord{}, err
		}
		payload, _ := json.Marshal(restartRequest{Executable: executable, Reply: filepath.Base(reply), Flags: flags})
		if err := atomicfile.WriteNew(restartRequestPath(ns.DataDir, previous), payload, atomicfile.WithPrivate()); err != nil {
			if errors.Is(err, os.ErrExist) {
				return kitdaemon.RuntimeRecord{}, fmt.Errorf("daemon pid %d already has a pending restart: %w", previous.PID, err)
			}
			return kitdaemon.RuntimeRecord{}, err
		}
	}
	if err := daemonruntime.SignalDaemonRestart(previous, ns.DBHash); err != nil {
		if reply != "" {
			_ = os.Remove(restartRequestPath(ns.DataDir, previous))
		}
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("signal daemon pid %d: %w", previous.PID, err)
	}
	return waitForDaemonReplacement(ctx, kitdaemon.RuntimeStore{Dir: ns.DataDir}, previous, reply)
}
