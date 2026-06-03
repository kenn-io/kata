package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/db"
)

// runRecord is the JSONL line shape for runs.jsonl. The dispatcher's
// runs appender (Task 6) marshals these.
type runRecord struct {
	Version          int    `json:"kata_hook_runs_version"`
	EventID          int64  `json:"event_id"`
	EventType        string `json:"event_type"`
	HookIndex        int    `json:"hook_index"`
	HookCommand      string `json:"hook_command"`
	StartedAt        string `json:"started_at"`
	EndedAt          string `json:"ended_at"`
	DurationMS       int64  `json:"duration_ms"`
	ExitCode         int    `json:"exit_code"`
	Result           string `json:"result"`
	StdoutPath       string `json:"stdout_path"`
	StderrPath       string `json:"stderr_path"`
	StdoutBytes      int64  `json:"stdout_bytes"`
	StderrBytes      int64  `json:"stderr_bytes"`
	SpawnError       string `json:"spawn_error"`
	PayloadTruncated bool   `json:"payload_truncated"`
}

// runDeps is what runJob needs from its caller. The dispatcher (Task 8)
// fills these from DispatcherDeps + its own per-instance state.
type runDeps struct {
	OutputDir   string
	DaemonLog   *log.Logger
	Now         func() time.Time
	GraceWindow time.Duration
	Project     projectResolver
	Issue       issueResolver
	Comment     commentResolver
	Alias       aliasResolver
	AppendRun   func(runRecord)
	// LogWorkingDirMissing is called by runJob when a hook's working_dir
	// is missing at fire time, so the dispatcher can rate-limit-log a
	// single line per spam window (master spec §8.8). Optional —
	// runJob no-ops if nil so unit tests don't have to wire it.
	LogWorkingDirMissing func(ResolvedHook)
}

// frozenAliasResolver returns a resolver that always reports the given
// snapshot/has/err triple. runJob hoists the real resolver call to one
// site and uses this wrapper to feed the result into buildStdinJSON
// without invoking the underlying resolver a second time.
func frozenAliasResolver(snap AliasSnapshot, has bool, err error) aliasResolver {
	return func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) {
		return snap, has, err
	}
}

// runContext holds the per-invocation state of one hook job. Methods on
// runContext encapsulate finalization and cleanup so runJob stays a
// short orchestrator.
type runContext struct {
	job       HookJob
	deps      runDeps
	outFile   *os.File
	errFile   *os.File
	outPath   string
	errPath   string
	startedAt time.Time
}

func newRunContext(job HookJob, deps runDeps) *runContext {
	return &runContext{
		job:       job,
		deps:      deps,
		outPath:   filepath.Join(deps.OutputDir, fmt.Sprintf("%d.%d.out", job.Event.ID, job.Hook.Index)),
		errPath:   filepath.Join(deps.OutputDir, fmt.Sprintf("%d.%d.err", job.Event.ID, job.Hook.Index)),
		startedAt: deps.Now(),
	}
}

// openOutputFiles creates the .out and .err capture files. On any
// failure both files are closed AND any successfully created file is
// removed from disk, so a half-created output never lingers as an
// untracked artifact while the runRecord reports empty paths.
func (rc *runContext) openOutputFiles() error {
	outFile, oErr := os.OpenFile(rc.outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is OutputDir + int64.int filename, daemon-controlled
	errFile, eErr := os.OpenFile(rc.errPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: path is OutputDir + int64.int filename, daemon-controlled
	if oErr != nil || eErr != nil {
		if outFile != nil {
			_ = outFile.Close()
			_ = os.Remove(rc.outPath)
		}
		if errFile != nil {
			_ = errFile.Close()
			_ = os.Remove(rc.errPath)
		}
		return fmt.Errorf("open output files: out=%v err=%v", oErr, eErr)
	}
	rc.outFile = outFile
	rc.errFile = errFile
	return nil
}

// closeFiles is a best-effort cleanup safety net; finalize() is the
// authoritative finalizer. After finalize runs, outFile and errFile
// are nil and these calls become no-ops.
func (rc *runContext) closeFiles() {
	_ = closeFile(rc.outFile)
	_ = closeFile(rc.errFile)
}

// finalize closes the capture files (so on-disk size is final),
// stat's them, and emits the runRecord.
func (rc *runContext) finalize(result, spawnErr string, exitCode int, payloadTruncated bool) {
	// Close before stat'ing so the on-disk size is final.
	_ = closeFile(rc.outFile)
	_ = closeFile(rc.errFile)
	rc.outFile = nil
	rc.errFile = nil

	var outBytes, errBytes int64
	if st, err := os.Stat(rc.outPath); err == nil {
		outBytes = st.Size()
	}
	if st, err := os.Stat(rc.errPath); err == nil {
		errBytes = st.Size()
	}
	ended := rc.deps.Now()
	rc.deps.AppendRun(runRecord{
		Version:          1,
		EventID:          rc.job.Event.ID,
		EventType:        rc.job.Event.Type,
		HookIndex:        rc.job.Hook.Index,
		HookCommand:      rc.job.Hook.Command,
		StartedAt:        rc.startedAt.UTC().Format(time.RFC3339Nano),
		EndedAt:          ended.UTC().Format(time.RFC3339Nano),
		DurationMS:       ended.Sub(rc.startedAt).Milliseconds(),
		ExitCode:         exitCode,
		Result:           result,
		StdoutPath:       rc.outPath,
		StderrPath:       rc.errPath,
		StdoutBytes:      outBytes,
		StderrBytes:      errBytes,
		SpawnError:       spawnErr,
		PayloadTruncated: payloadTruncated,
	})
}

func closeFile(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}

// classifyWorkingDir checks fire-time existence/type of the working
// dir and translates failures into runRecord result/spawn_error
// strings. Returns ("", "") if the directory is usable. Distinct from
// the config-load-time validateWorkingDir (which only checks shape).
func classifyWorkingDir(path string) (result, spawnErr string) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "working_dir_missing", err.Error()
		}
		return "spawn_failed", err.Error()
	}
	if !st.IsDir() {
		return "spawn_failed", "working_dir is not a directory"
	}
	return "", ""
}

// waitForCompletion owns the doneCh goroutine, the timeout timer, and
// the 3-way select between normal exit, timeout, and daemon shutdown.
// On timeout/shutdown branches it kills the process group with grace
// and drains doneCh so the wait goroutine exits cleanly.
type completionAction int

const (
	completionDone completionAction = iota
	completionTimeout
	completionShutdown
)

func waitForCompletion(cmd *exec.Cmd, timeout time.Duration, shutdown <-chan struct{}, deps runDeps) (result string, exitCode int) {
	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	result, exitCode, action := selectCompletion(doneCh, timer.C, shutdown)
	switch action {
	case completionDone:
		return result, exitCode
	case completionTimeout:
		killTreeWithGrace(cmd, deps.GraceWindow, deps.DaemonLog)
		w := <-doneCh
		return "timed_out", exitCodeOf(w)
	case completionShutdown:
		killTreeWithGrace(cmd, deps.GraceWindow, deps.DaemonLog)
		w := <-doneCh
		return "daemon_shutdown", exitCodeOf(w)
	default:
		panic("unknown hook completion action")
	}
}

func selectCompletion(
	doneCh <-chan error,
	timeout <-chan time.Time,
	shutdown <-chan struct{},
) (result string, exitCode int, action completionAction) {
	if result, exitCode, ok := pollCompletion(doneCh); ok {
		return result, exitCode, completionDone
	}
	select {
	case e := <-doneCh:
		return "ok", exitCodeOf(e), completionDone
	case <-timeout:
		if result, exitCode, ok := pollCompletion(doneCh); ok {
			return result, exitCode, completionDone
		}
		return "timed_out", -1, completionTimeout
	case <-shutdown:
		if result, exitCode, ok := pollCompletion(doneCh); ok {
			return result, exitCode, completionDone
		}
		return "daemon_shutdown", -1, completionShutdown
	}
}

func pollCompletion(doneCh <-chan error) (result string, exitCode int, ok bool) {
	select {
	case e := <-doneCh:
		return "ok", exitCodeOf(e), true
	default:
		return "", 0, false
	}
}

func runJob(ctx context.Context, shutdown <-chan struct{}, job HookJob, deps runDeps) {
	rc := newRunContext(job, deps)
	if err := rc.openOutputFiles(); err != nil {
		rc.finalize("spawn_failed", err.Error(), -1, false)
		return
	}
	defer rc.closeFiles()

	if result, spawnErr := classifyWorkingDir(job.Hook.WorkingDir); result != "" {
		if result == "working_dir_missing" && deps.LogWorkingDirMissing != nil {
			deps.LogWorkingDirMissing(job.Hook)
		}
		rc.finalize(result, spawnErr, -1, false)
		return
	}

	// Resolve the alias once and reuse for stdin payload AND env vars
	// (spec §6.1). Doubling the resolver call would double DB load and
	// risk inconsistent results if the underlying alias mutated.
	// The frozen wrapper still reports aliasErr so buildAliasBlock can
	// log it; buildEnv only sees the gated `useAlias` so a resolver
	// that returns (snap, true, err) doesn't leak KATA_ALIAS_* env
	// vars while the stdin payload omits the alias block.
	asnap, hasAlias, aliasErr := deps.Alias(ctx, job.Event)
	useAlias := hasAlias && aliasErr == nil
	logf := func(format string, args ...any) { deps.DaemonLog.Printf(format, args...) }
	stdinPayload, payloadTruncated := buildStdinJSON(ctx, job.Event, deps.Project, deps.Issue, deps.Comment,
		frozenAliasResolver(asnap, hasAlias, aliasErr), logf)

	cmd := exec.Command(job.Hook.Command, job.Hook.Args...) //nolint:gosec // G204: command validated at config load
	cmd.Dir = job.Hook.WorkingDir
	cmd.Env = buildEnv(job.Hook.UserEnv, job.Event, asnap, useAlias)
	cmd.Stdin = bytes.NewReader(stdinPayload)
	cmd.Stdout = rc.outFile
	cmd.Stderr = rc.errFile
	applyProcessGroupAttrs(cmd)

	if err := cmd.Start(); err != nil {
		rc.finalize("spawn_failed", err.Error(), -1, payloadTruncated)
		return
	}

	result, exitCode := waitForCompletion(cmd, job.Hook.Timeout, shutdown, deps)
	rc.finalize(result, "", exitCode, payloadTruncated)
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// buildEnv composes the child process's environment from os.Environ ⊕
// the hook's user-defined env ⊕ the KATA_* contract vars. Alias-related
// env is fed by the caller (runJob) which resolved it once for both
// the stdin payload and this env slice.
func buildEnv(userEnv []string, evt db.Event, asnap AliasSnapshot, hasAlias bool) []string {
	env := append([]string{}, os.Environ()...)
	env = append(env, userEnv...)
	env = append(env,
		"KATA_HOOK_VERSION=1",
		"KATA_EVENT_ID="+strconv.FormatInt(evt.ID, 10),
		"KATA_EVENT_TYPE="+evt.Type,
		"KATA_ACTOR="+evt.Actor,
		"KATA_CREATED_AT="+evt.CreatedAt.UTC().Format(time.RFC3339Nano),
		"KATA_PROJECT_ID="+strconv.FormatInt(evt.ProjectID, 10),
		"KATA_PROJECT_NAME="+evt.ProjectName,
	)
	if evt.IssueUID != nil {
		env = append(env, "KATA_ISSUE_UID="+*evt.IssueUID)
	}
	if hasAlias {
		env = append(env,
			"KATA_ALIAS_IDENTITY="+asnap.Identity,
		)
	}
	return env
}

// killTreeWithGrace asks the leader's process group to terminate
// (SIGTERM on Unix; Process.Kill on Windows where there is no group).
// After the grace window it unconditionally escalates to SIGKILL on
// the group: children that ignored SIGTERM, or orphans whose leader
// already exited, still need to be torn down. Errors are logged via
// daemonLog and never surface — the runner has already classified
// the result by the time we get here.
func killTreeWithGrace(cmd *exec.Cmd, grace time.Duration, daemonLog *log.Logger) {
	if cmd.Process == nil {
		return
	}
	if err := terminateGroup(cmd); err != nil {
		daemonLog.Printf("hooks: terminate: %v", err)
	}
	if !waitGroupGone(cmd, grace) {
		if err := killGroup(cmd); err != nil {
			daemonLog.Printf("hooks: kill: %v", err)
		}
	}
}

// waitGroupGone returns true once the leader's process group has no
// remaining members, or false on grace expiry. On platforms where
// liveness can't be observed (Windows), it returns false after grace
// so the caller escalates to a force kill.
func waitGroupGone(cmd *exec.Cmd, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !groupAlive(cmd) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !groupAlive(cmd)
}
