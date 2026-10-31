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
	"strings"
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
	reply      string
	requested  atomic.Bool
}

// daemonRestartReplyEnv names the accepted request in the replacement's environment.
const daemonRestartReplyEnv = "KATA_DAEMON_RESTART_REPLY"

// startedForRestartReply is unset at process start so children never inherit it.
var startedForRestartReply = takeRestartReply()

func takeRestartReply() string {
	reply := os.Getenv(daemonRestartReplyEnv)
	_ = os.Unsetenv(daemonRestartReplyEnv)
	return reply
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
				logger.Printf("restart: request refused: %v", err)
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
	if err := reexecDaemon(ctx, r.executable, r.args, append(os.Environ(), daemonRestartReplyEnv+"="+r.reply)); err != nil {
		return fmt.Errorf("restart daemon: %w", err)
	}
	return nil
}

// metadata advertises restart support and the request this daemon was started for.
func (r *daemonRestart) metadata() map[string]string {
	if r == nil {
		return nil
	}
	return map[string]string{
		daemonruntime.RuntimeRestartMetadataKey:   "1",
		daemonruntime.RuntimeRestartExecutableKey: "1",
		daemonruntime.RuntimeRestartReplyKey:      startedForRestartReply,
	}
}

// daemonArgs returns this daemon's arguments with extra flags replacing any
// earlier values of the same flags, ahead of a "--" delimiter.
func daemonArgs(extra ...string) []string {
	args := withoutFlags(os.Args[1:], extra)
	if at := slices.Index(args, "--"); at >= 0 {
		return slices.Insert(args, at, extra...)
	}
	return append(args, extra...)
}

// withoutFlags copies args without the flags that overrides name, leaving
// everything from "--" on. Daemon start takes no positional arguments, so a
// non-flag token after a named flag is that flag's separate value.
func withoutFlags(args, overrides []string) []string {
	names := make([]string, 0, len(overrides))
	for _, override := range overrides {
		name, _, _ := strings.Cut(strings.TrimPrefix(override, "--"), "=")
		names = append(names, name)
	}
	kept := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return append(kept, args[i:]...)
		}
		name, _, inline := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") || !slices.Contains(names, name) {
			kept = append(kept, args[i])
			continue
		}
		if !inline && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return kept
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

// accept applies the restart request published for rec and writes any refusal
// to its caller's reply file.
func (r *daemonRestart) accept(ctx context.Context, dir string, rec kitdaemon.RuntimeRecord) (err error) {
	var req restartRequest
	defer func() {
		reply := filepath.Join(dir, filepath.Base(req.Reply))
		if _, statErr := os.Lstat(reply); err != nil && req.Reply != "" && statErr == nil {
			_ = atomicfile.WriteFile(reply, []byte(err.Error()), atomicfile.WithPrivate())
		}
	}()
	executable := r.executable
	request := restartRequestPath(dir, rec)
	target, err := os.ReadFile(request) //nolint:gosec // G304: generation-scoped request in the daemon's runtime directory
	switch {
	case err == nil:
		if err := os.Remove(request); err != nil {
			return err
		}
		if err := json.Unmarshal(target, &req, json.RejectUnknownMembers(true)); err != nil {
			return err
		}
		if req.Executable != "" {
			executable, err = exec.LookPath(req.Executable)
			if err != nil {
				return err
			}
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := validateDaemonReplacement(ctx, executable, req.Flags); err != nil {
		return err
	}
	r.executable, r.args, r.reply = executable, daemonArgs(req.Flags...), req.Reply
	return nil
}

func restartRequestPath(dir string, rec kitdaemon.RuntimeRecord) string {
	return filepath.Join(dir, fmt.Sprintf("restart.%d.%d", rec.PID, rec.StartedAt.UnixNano()))
}

// requestDaemonRestart asks the live daemon to restart within one restart wait
// budget, reporting a timeout as the daemon not restarting in time.
func requestDaemonRestart(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord, executable string, flags []string) (kitdaemon.RuntimeRecord, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, daemonRestartProcessWaitTimeout, fmt.Errorf("daemon pid %d did not restart within %s", previous.PID, daemonRestartProcessWaitTimeout))
	defer cancel()
	replacement, err := requestDaemonRestartOnce(ctx, ns, previous, executable, flags)
	if errors.Is(err, context.DeadlineExceeded) {
		err = context.Cause(ctx)
	}
	return replacement, err
}

// lockLiveDaemons takes the runtime store's start lock and lists the live
// daemons under it. Restart callers hold the lock until their replacement is
// ready, so no caller chooses while a daemon is between records.
func lockLiveDaemons(ctx context.Context, ns *daemonruntime.Namespace, stderr io.Writer) ([]kitdaemon.RuntimeRecord, func(), error) {
	store := kitdaemon.RuntimeStore{Dir: ns.DataDir}
	release, err := store.AcquireStartLock(ctx)
	if err != nil {
		return nil, nil, err
	}
	records, err := store.List()
	if err != nil {
		release()
		return nil, nil, err
	}
	noteSkippedDaemons(stderr, records)
	return slices.DeleteFunc(records, func(record kitdaemon.RuntimeRecord) bool {
		return !daemonruntime.RuntimeProcessAlive(record)
	}), release, nil
}

func requestDaemonRestartOnce(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord, executable string, flags []string) (kitdaemon.RuntimeRecord, error) {
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
		if err := atomicfile.WriteFile(restartRequestPath(ns.DataDir, previous), payload, atomicfile.WithPrivate()); err != nil {
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
