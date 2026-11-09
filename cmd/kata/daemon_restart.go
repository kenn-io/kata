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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/daemonruntime"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/consolewindow"
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
	dir        string
	reply      string
	from       string
	requested  atomic.Bool
}

const (
	// daemonRestartReplyEnv names the accepted request in the replacement's environment.
	daemonRestartReplyEnv = "KATA_DAEMON_RESTART_REPLY"
	// daemonRestartFromEnv names the daemon generation the replacement replaces.
	daemonRestartFromEnv = "KATA_DAEMON_RESTART_FROM"
	// daemonReplacementCheckTimeout bounds the replacement's startup check,
	// which can be slow the first time a newly installed binary runs.
	daemonReplacementCheckTimeout = 30 * time.Second
)

// The restart variables are unset at process start so children never inherit them.
var (
	startedForRestartReply = takeEnv(daemonRestartReplyEnv)
	startedForRestartFrom  = takeEnv(daemonRestartFromEnv)
)

func takeEnv(key string) string {
	value := os.Getenv(key)
	_ = os.Unsetenv(key)
	return value
}

// resolveDaemonExecutable is swapped in tests that exercise unsupported hosts.
var resolveDaemonExecutable = os.Executable

func init() {
	client.RestartLocalDaemon = func(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord) (kitdaemon.RuntimeRecord, error) {
		restart := newDaemonRestart(io.Discard)
		if restart == nil {
			return kitdaemon.RuntimeRecord{}, errors.New("cannot resolve installed executable for daemon replacement")
		}
		return requestDaemonRestart(ctx, ns, previous, restart.executable, nil)
	}
}

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
// A non-nil idle admission is held from the signal until the restart is
// recorded, so an idle deadline cannot cancel validation and stop the daemon
// without a replacement.
func (r *daemonRestart) watch(ctx context.Context, sigs <-chan os.Signal, idle daemon.IdleForegroundAdmission, validate func() error, logger *log.Logger, trigger func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			if r.handle(idle, validate, logger, trigger) {
				return
			}
		}
	}
}

func (r *daemonRestart) handle(idle daemon.IdleForegroundAdmission, validate func() error, logger *log.Logger, trigger func()) bool {
	if idle != nil {
		lease, admitted := idle.TryForeground()
		if !admitted {
			logger.Printf("restart: request refused: daemon is shutting down")
			return false
		}
		defer lease.Release()
	}
	if err := validate(); err != nil {
		logger.Printf("restart: request refused: %v", err)
		return false
	}
	r.requested.Store(true)
	trigger()
	return true
}

// exec replaces the daemon once the current process has shut down. On Unix
// the process image is replaced in place so supervisors keep the same PID; on
// Windows a detached replacement starts and this process exits.
func (r *daemonRestart) exec(ctx context.Context) error {
	if r == nil || !r.requested.Load() {
		return nil
	}
	env := append(os.Environ(), daemonRestartReplyEnv+"="+r.reply, daemonRestartFromEnv+"="+r.from)
	started := func(pid int) {
		writeRestartReply(r.dir, r.reply, restartReplacementPIDSuffix, []byte(strconv.Itoa(pid)))
	}
	if err := reexecDaemon(ctx, r.executable, r.args, env, started); err != nil {
		return reportRestartFailure(r.reply, fmt.Errorf("restart daemon: %w", err))
	}
	return nil
}

// metadata advertises restart support and the request this daemon was started
// for. A daemon without an installed executable says so, which distinguishes
// it from an older release that cannot accept executable requests.
func (r *daemonRestart) metadata() map[string]string {
	metadata := map[string]string{
		daemonruntime.RuntimeRestartExecutableKey: "0",
		daemonruntime.RuntimeRestartReplyKey:      startedForRestartReply,
		daemonruntime.RuntimeRestartFromKey:       startedForRestartFrom,
	}
	if r != nil {
		metadata[daemonruntime.RuntimeRestartMetadataKey] = "1"
		metadata[daemonruntime.RuntimeRestartExecutableKey] = "1"
	}
	return metadata
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
	ctx, cancel := context.WithTimeout(ctx, daemonReplacementCheckTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable, daemonArgs(append(flags, "--check-startup")...)...) //nolint:gosec // selected installed executable with daemon-owned arguments
	consolewindow.Hide(command)
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
	defer func() { writeRestartFailure(dir, req.Reply, err) }()
	executable := r.executable
	request := restartRequestPath(dir, rec)
	target, err := os.ReadFile(request) //nolint:gosec // G304: generation-scoped request in the daemon's runtime directory
	switch {
	case err == nil:
		if err := os.Remove(request); err != nil {
			return err
		}
		// Unknown members are ignored so a newer client can extend the request
		// without every daemon from this release refusing it.
		if err := json.Unmarshal(target, &req); err != nil {
			return err
		}
		if req.Executable != "" {
			executable = req.Executable
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := validateDaemonReplacement(ctx, executable, req.Flags); err != nil {
		return err
	}
	r.executable, r.args, r.reply = executable, daemonArgs(req.Flags...), req.Reply
	r.dir, r.from = dir, restartGeneration(rec)
	return nil
}

// restartReplacementPIDSuffix names the file beside a caller's reply where a
// Windows daemon records the separate replacement process it started.
const restartReplacementPIDSuffix = ".pid"

// writeRestartReply answers the caller waiting on reply, if it still waits.
func writeRestartReply(dir, reply, suffix string, content []byte) {
	if reply == "" {
		return
	}
	path := filepath.Join(dir, filepath.Base(reply))
	if _, err := os.Lstat(path); err == nil {
		_ = atomicfile.WriteFile(path+suffix, content, atomicfile.WithPrivate())
	}
}

func writeRestartFailure(dir, reply string, err error) {
	if err != nil {
		writeRestartReply(dir, reply, "", []byte(err.Error()))
	}
}

func reportRestartFailure(reply string, err error) error {
	if reply != "" {
		if ns, nsErr := daemonruntime.NewNamespace(); nsErr == nil {
			writeRestartFailure(ns.DataDir, reply, fmt.Errorf("replacement failed to start: %w", err))
		}
	}
	return err
}

// restartGeneration identifies one daemon process start; Unix keeps the PID
// across in-place restarts.
func restartGeneration(rec kitdaemon.RuntimeRecord) string {
	return fmt.Sprintf("%d.%d", rec.PID, rec.StartedAt.UnixNano())
}

func restartRequestPath(dir string, rec kitdaemon.RuntimeRecord) string {
	return filepath.Join(dir, "restart."+restartGeneration(rec))
}

// removeAbandonedRestartFiles deletes requests and replies left by restart
// callers that were killed. Every caller holds the start lock while its files
// exist, so none can belong to a caller that is still waiting.
func removeAbandonedRestartFiles(dir string) {
	for _, pattern := range []string{"restart-reply-*", "restart.[0-9]*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, match := range matches {
			_ = os.Remove(match)
		}
	}
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

// requestDaemonRestart keeps the caller waiting through replacement readiness,
// including SQLite migrations, once the restart request begins. The caller
// holds the runtime store's start lock.
func requestDaemonRestart(ctx context.Context, ns *daemonruntime.Namespace, previous kitdaemon.RuntimeRecord, executable string, flags []string) (kitdaemon.RuntimeRecord, error) {
	ctx = context.WithoutCancel(ctx)
	removeAbandonedRestartFiles(ns.DataDir)
	var reply string
	if previous.Metadata[daemonruntime.RuntimeRestartExecutableKey] == "1" {
		file, err := safefileio.CreatePrivateTemp(ns.DataDir, "restart-reply-*")
		if err != nil {
			return kitdaemon.RuntimeRecord{}, err
		}
		reply = file.Name()
		defer func() {
			_ = os.Remove(reply)
			_ = os.Remove(reply + restartReplacementPIDSuffix)
		}()
		if err := file.Close(); err != nil {
			return kitdaemon.RuntimeRecord{}, err
		}
		payload, _ := json.Marshal(restartRequest{Executable: executable, Reply: filepath.Base(reply), Flags: flags})
		if err := atomicfile.WriteFile(restartRequestPath(ns.DataDir, previous), payload, atomicfile.WithPrivate()); err != nil {
			return kitdaemon.RuntimeRecord{}, err
		}
		defer func() { _ = os.Remove(restartRequestPath(ns.DataDir, previous)) }()
	}
	if err := daemonruntime.SignalDaemonRestart(previous, ns.DBHash); err != nil {
		return kitdaemon.RuntimeRecord{}, fmt.Errorf("signal daemon pid %d: %w", previous.PID, err)
	}
	return waitForDaemonReplacement(ctx, kitdaemon.RuntimeStore{Dir: ns.DataDir}, previous, reply)
}

func restartRunningDaemons(ctx context.Context, ns *daemonruntime.Namespace, running []kitdaemon.RuntimeRecord, flags []string) ([]int, daemonStartOutput, error) {
	var out daemonStartOutput
	pids := make([]int, 0, len(running))
	if len(running) == 0 {
		return pids, out, nil
	}
	target := newDaemonRestart(io.Discard)
	if target == nil {
		return nil, out, errors.New("cannot resolve the installed restart executable")
	}
	for _, rec := range running {
		replacement, err := requestDaemonRestart(ctx, ns, rec, target.executable, flags)
		if err != nil {
			return nil, out, err
		}
		pids = append(pids, rec.PID)
		out = daemonStartOutputFromRecord("restarted", replacement)
	}
	return pids, out, nil
}

func requireRestartable(running []kitdaemon.RuntimeRecord) error {
	for _, rec := range running {
		if rec.Metadata[daemonruntime.RuntimeRestartExecutableKey] != "1" {
			return unsupportedDaemonRestartError(rec)
		}
	}
	return nil
}
