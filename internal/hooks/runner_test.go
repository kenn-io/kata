package hooks

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/kata/internal/db"
)

type runnerSetup struct {
	t      *testing.T
	deps   runDeps
	dir    string
	dbHash string
	logBuf *strings.Builder
}

func newRunnerSetup(t *testing.T) *runnerSetup {
	t.Helper()
	root := t.TempDir()
	dbHash := "testdbhash01"
	outDir := filepath.Join(root, "hooks", dbHash, "output")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logBuf := &strings.Builder{}
	logger := log.New(logBuf, "", 0)
	deps := runDeps{
		OutputDir:   outDir,
		DaemonLog:   logger,
		Now:         func() time.Time { return time.Date(2026, 4, 30, 14, 22, 11, 0, time.UTC) },
		GraceWindow: 100 * time.Millisecond,
		Project:     okProject,
		Issue:       okIssue,
		Comment:     okComment,
		Alias:       okAlias,
		AppendRun:   func(_ runRecord) {},
	}
	return &runnerSetup{t: t, deps: deps, dir: root, dbHash: dbHash, logBuf: logBuf}
}

// runProbe executes hookprobe with default ResolvedHook fields
// (Command=hookprobe, Timeout=10s, WorkingDir=rs.dir), applying customize
// to override fields, and returns the captured runRecord. Callers that
// only need to vary args can pass a one-liner customize.
func (rs *runnerSetup) runProbe(customize func(*ResolvedHook)) runRecord {
	rs.t.Helper()
	return rs.runProbeWithDone(make(chan struct{}), customize)
}

// runProbeWithDone is the full-control variant taking a caller-supplied
// shutdown channel; used by daemon-shutdown tests.
func (rs *runnerSetup) runProbeWithDone(done chan struct{}, customize func(*ResolvedHook)) runRecord {
	rs.t.Helper()
	var got runRecord
	rs.deps.AppendRun = func(r runRecord) { got = r }
	hook := ResolvedHook{
		Command:    hookprobePath(rs.t),
		Timeout:    10 * time.Second,
		WorkingDir: rs.dir,
	}
	if customize != nil {
		customize(&hook)
	}
	job := HookJob{
		Event:      sampleEvent("issue.created"),
		Hook:       hook,
		EnqueuedAt: rs.deps.Now(),
	}
	runJob(context.Background(), done, job, rs.deps)
	return got
}

func readRecordOut(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test-controlled path under t.TempDir()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestRunner_OK_HookprobeStdin(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"stdin"} })
	if got.Result != "ok" {
		t.Fatalf("result = %q, want ok (log=%s)", got.Result, rs.logBuf.String())
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit_code = %d, want 0", got.ExitCode)
	}
	stdout := readRecordOut(t, got.StdoutPath)
	if !strings.Contains(stdout, `"event_id":81237`) {
		t.Fatalf("stdout missing event_id: %q", stdout)
	}
}

func TestRunner_NonzeroExit(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"exit", "7"} })
	if got.Result != "ok" || got.ExitCode != 7 {
		t.Fatalf("got %+v, want result=ok exit_code=7", got)
	}
}

func TestRunner_SpawnFailed_NonexistentCommand(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) { h.Command = "/nonexistent/no-such-binary" })
	if got.Result != "spawn_failed" {
		t.Fatalf("result = %q, want spawn_failed", got.Result)
	}
	if got.StdoutPath == "" || got.StderrPath == "" {
		t.Fatalf("paths should still be recorded: %+v", got)
	}
	if got.StdoutBytes != 0 || got.StderrBytes != 0 {
		t.Fatalf("byte counts should be 0 on spawn_failed: %+v", got)
	}
}

func TestRunner_WorkingDirMissing(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Args = []string{"exit", "0"}
		h.WorkingDir = filepath.Join(rs.dir, "nope")
	})
	if got.Result != "working_dir_missing" {
		t.Fatalf("result = %q, want working_dir_missing", got.Result)
	}
}

// TestRunner_AliasResolverInvokedOnce pins the spec §6.1 contract that
// the alias resolver is called exactly once per hook fire — its result
// is shared between the stdin payload (buildAliasBlock) and the env
// vars (buildEnv). A naïve implementation calls it twice and doubles
// DB load.
func TestRunner_AliasResolverInvokedOnce(t *testing.T) {
	rs := newRunnerSetup(t)
	var calls int32
	rs.deps.Alias = func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) {
		atomic.AddInt32(&calls, 1)
		return AliasSnapshot{Identity: "github.com/wesm/kata", Kind: "git"}, true, nil
	}
	rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"exit", "0"} })
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("alias resolver invocations = %d, want 1", got)
	}
}

// TestRunner_AliasResolverErr_NoEnvLeak pins the contract that a
// resolver returning (snap, hasAlias=true, err) must NOT export
// KATA_ALIAS_* env vars: stdin's buildAliasBlock omits the alias
// block on err, so env must agree to keep the two views consistent.
func TestRunner_AliasResolverErr_NoEnvLeak(t *testing.T) {
	rs := newRunnerSetup(t)
	rs.deps.Alias = func(_ context.Context, _ db.Event) (AliasSnapshot, bool, error) {
		return AliasSnapshot{Identity: "github.com/wesm/kata", Kind: "git"},
			true, errors.New("resolver boom")
	}
	got := rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"env", "KATA_ALIAS_IDENTITY"} })
	if got.Result != "ok" {
		t.Fatalf("expected ok, got %q", got.Result)
	}
	out := readRecordOut(t, got.StdoutPath)
	if strings.TrimSpace(out) != "" {
		t.Fatalf("KATA_ALIAS_IDENTITY must be empty when resolver returned err: %q", out)
	}
}

// TestRunner_WorkingDirMissing_LogsViaCallback pins the master spec
// §8.8 contract that working_dir_missing emits a daemonLog warning
// (rate-limited by the dispatcher; the runner just calls the callback
// once per occurrence).
func TestRunner_WorkingDirMissing_LogsViaCallback(t *testing.T) {
	rs := newRunnerSetup(t)
	var loggedHook ResolvedHook
	var logged int32
	rs.deps.LogWorkingDirMissing = func(h ResolvedHook) {
		atomic.AddInt32(&logged, 1)
		loggedHook = h
	}
	missing := filepath.Join(rs.dir, "absent")
	rs.runProbe(func(h *ResolvedHook) {
		h.Index = 7
		h.Args = []string{"exit", "0"}
		h.WorkingDir = missing
	})
	if atomic.LoadInt32(&logged) != 1 {
		t.Fatalf("LogWorkingDirMissing should be called exactly once, got %d", logged)
	}
	if loggedHook.Index != 7 || loggedHook.WorkingDir != missing {
		t.Fatalf("callback got hook=%+v, want index=7 dir=%q", loggedHook, missing)
	}
}

func TestRunner_WorkingDirIsFile(t *testing.T) {
	rs := newRunnerSetup(t)
	wd := filepath.Join(rs.dir, "wd-as-file")
	if err := os.WriteFile(wd, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Args = []string{"exit", "0"}
		h.WorkingDir = wd
	})
	if got.Result != "spawn_failed" {
		t.Fatalf("working_dir = file: result = %q, want spawn_failed", got.Result)
	}
}

func TestRunner_TimedOut_TermDelay(t *testing.T) {
	rs := newRunnerSetup(t)
	rs.deps.GraceWindow = 200 * time.Millisecond
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Args = []string{"term-delay", "10ms"}
		h.Timeout = 50 * time.Millisecond
	})
	if got.Result != "timed_out" {
		t.Fatalf("result = %q, want timed_out", got.Result)
	}
}

func TestRunner_TimedOut_TermIgnore_Killed(t *testing.T) {
	rs := newRunnerSetup(t)
	rs.deps.GraceWindow = 50 * time.Millisecond
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Args = []string{"term-ignore", "10s"}
		h.Timeout = 50 * time.Millisecond
	})
	if got.Result != "timed_out" {
		t.Fatalf("result = %q, want timed_out (SIGKILL fallback)", got.Result)
	}
}

func TestRunner_DaemonShutdown_BeforeWait(t *testing.T) {
	rs := newRunnerSetup(t)
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()
	got := rs.runProbeWithDone(done, func(h *ResolvedHook) {
		h.Args = []string{"sleep", "1s"}
		h.Timeout = 5 * time.Second
	})
	if got.Result != "daemon_shutdown" {
		t.Fatalf("result = %q, want daemon_shutdown", got.Result)
	}
}

func TestCompletionSelectPrefersDoneOverExpiredTimeout(t *testing.T) {
	done := make(chan error, 1)
	done <- nil
	expired := make(chan time.Time, 1)
	expired <- time.Now()

	result, exitCode, action := selectCompletion(done, expired, make(chan struct{}))

	if result != "ok" || exitCode != 0 || action != completionDone {
		t.Fatalf("got result=%q exit=%d action=%v, want ok/0/done", result, exitCode, action)
	}
}

func TestRunner_OutputCapture_BothStreams(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"both", "OUT", "ERR"} })
	out := readRecordOut(t, got.StdoutPath)
	er := readRecordOut(t, got.StderrPath)
	if !strings.Contains(out, "OUT") {
		t.Fatalf(".out missing OUT: %q", out)
	}
	if !strings.Contains(er, "ERR") {
		t.Fatalf(".err missing ERR: %q", er)
	}
	if got.StdoutBytes != int64(len(out)) || got.StderrBytes != int64(len(er)) {
		t.Fatalf("recorded sizes don't match disk: %+v vs %d/%d", got, len(out), len(er))
	}
}

func TestRunner_EnvKataVars(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) { h.Args = []string{"env", "KATA_EVENT_ID"} })
	out := readRecordOut(t, got.StdoutPath)
	if strings.TrimSpace(out) != "81237" {
		t.Fatalf("KATA_EVENT_ID = %q, want 81237", out)
	}
}

func TestRunner_EnvUserOverridable_NotForKata(t *testing.T) {
	rs := newRunnerSetup(t)
	got := rs.runProbe(func(h *ResolvedHook) {
		h.Args = []string{"env", "EXTRA"}
		h.UserEnv = []string{"EXTRA=visible"}
	})
	out := readRecordOut(t, got.StdoutPath)
	if strings.TrimSpace(out) != "visible" {
		t.Fatalf("user env not visible: %q", out)
	}
}
