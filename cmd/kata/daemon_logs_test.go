package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func writeRuns(t *testing.T, dir string, files map[string][]map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, lines := range files {
		writeHookLog(t, dir, name, lines...)
	}
}

// writeHookLog writes JSONL entries to dir/filename, creating dir if needed.
// Each entry is JSON-marshaled and terminated with a newline.
func writeHookLog(t *testing.T, dir, filename string, entries ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for _, e := range entries {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, filename), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// appendHookLogAsync schedules a background goroutine that, after delay,
// appends JSONL entries to path (creating the file if missing). Used by
// --tail tests that simulate logs arriving while a command is executing.
// Errors are silently ignored — the test will fail naturally if the data
// never lands.
func appendHookLogAsync(path string, delay time.Duration, entries ...map[string]any) {
	go func() {
		time.Sleep(delay)
		var buf bytes.Buffer
		for _, e := range entries {
			b, err := json.Marshal(e)
			if err != nil {
				return
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600) //nolint:gosec // G304: test-controlled temp path
		if err != nil {
			return
		}
		_, _ = f.Write(buf.Bytes())
		_ = f.Close()
	}()
}

// setupHooksDir initializes a temporary KATA_HOME with an empty kata.db,
// creates the namespaced hooks directory, and returns home, hooksDir, and
// the db hash used to derive hooksDir.
func setupHooksDir(t *testing.T) (home, hooksDir, dbHash string) {
	t.Helper()
	home = setupKataEnv(t)
	if err := os.WriteFile(filepath.Join(home, "kata.db"), []byte{0}, 0o600); err != nil {
		t.Fatal(err)
	}
	dbHash = config.DBHash(filepath.Join(home, "kata.db"))
	hooksDir = filepath.Join(home, "hooks", dbHash)
	if err := os.MkdirAll(hooksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return
}

func TestDaemonLogs_Hooks_PrintsChronological(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	writeRuns(t, dir, map[string][]map[string]any{
		"runs.jsonl.2": {{"event_id": 1, "result": "ok"}},
		"runs.jsonl.1": {{"event_id": 2, "result": "ok"}},
		"runs.jsonl":   {{"event_id": 3, "result": "ok"}, {"event_id": 4, "result": "ok"}},
	})
	resetFlags(t)
	out, _, err := executeRootCapture(t, context.Background(), "daemon", "logs", "--hooks")
	require.NoError(t, err)
	idx1 := strings.Index(out, `"event_id":1`)
	idx2 := strings.Index(out, `"event_id":2`)
	idx3 := strings.Index(out, `"event_id":3`)
	idx4 := strings.Index(out, `"event_id":4`)
	if idx1 >= idx2 || idx2 >= idx3 || idx3 >= idx4 {
		t.Fatalf("chronological order violated: %v %v %v %v", idx1, idx2, idx3, idx4)
	}
}

func TestDaemonLogs_Hooks_FailedOnly(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	writeRuns(t, dir, map[string][]map[string]any{
		"runs.jsonl": {
			{"event_id": 1, "result": "ok", "exit_code": 0},
			{"event_id": 2, "result": "ok", "exit_code": 7},
			{"event_id": 3, "result": "timed_out", "exit_code": -1},
		},
	})
	resetFlags(t)
	out, _, err := executeRootCapture(t, context.Background(), "daemon", "logs", "--hooks", "--failed-only")
	require.NoError(t, err)
	if strings.Contains(out, `"event_id":1`) {
		t.Fatal("--failed-only should exclude ok exit_code=0")
	}
	if !strings.Contains(out, `"event_id":2`) || !strings.Contains(out, `"event_id":3`) {
		t.Fatal("--failed-only should include nonzero exit and timed_out")
	}
}

func TestDaemonLogs_Hooks_MalformedLineSkippedWithStderrWarning(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	contents := "{\"event_id\":1,\"result\":\"ok\"}\nnot-json\n{\"event_id\":2,\"result\":\"ok\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(), "daemon", "logs", "--hooks")
	require.NoError(t, err)
	if !strings.Contains(stdout, `"event_id":1`) || !strings.Contains(stdout, `"event_id":2`) {
		t.Fatal("valid lines should still print")
	}
	if !strings.Contains(stderr, "skipping malformed line") {
		t.Fatalf("stderr should warn about malformed line: %q", stderr)
	}
}

func TestDaemonLogs_Hooks_AgentSkipsMalformedLine(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	contents := "{\"event_id\":1,\"result\":\"ok\"}\nnot-json\n{\"event_id\":2,\"result\":\"ok\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	resetFlags(t)
	stdout, stderr, err := executeRootCapture(t, context.Background(), "--agent", "daemon", "logs", "--hooks")
	require.NoError(t, err)

	assert.Contains(t, stdout, "OK daemon_log event_id=1 result=ok")
	assert.Contains(t, stdout, "OK daemon_log event_id=2 result=ok")
	assert.NotContains(t, stdout, "not-json")
	assert.NotContains(t, stdout, "raw=")
	assert.Contains(t, stderr, "skipping malformed line")
}

func TestDaemonLogs_Hooks_AgentOutputOneLinePerRecord(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	writeRuns(t, dir, map[string][]map[string]any{
		"runs.jsonl": {{
			"event_id":   1,
			"event_type": "issue.created",
			"hook_index": 0,
			"result":     "failed",
			"exit_code":  1,
			"stderr":     "first line\nsecond line",
		}},
	})
	resetFlags(t)

	stdout, _, err := executeRootCapture(t, context.Background(), "--agent", "daemon", "logs", "--hooks")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	require.Len(t, lines, 1, "agent daemon logs must emit one physical line per record: %q", stdout)
	assert.Contains(t, lines[0], "OK daemon_log")
	assert.Contains(t, lines[0], "event_id=1")
	assert.Contains(t, lines[0], "event_type=issue.created")
	assert.Contains(t, lines[0], "hook_index=0")
	assert.Contains(t, lines[0], "result=failed")
	assert.Contains(t, lines[0], "exit_code=1")
	assert.Contains(t, lines[0], `stderr="first line\nsecond line"`)
	assert.NotContains(t, lines[0], "{")
}

func TestFormatAgentHookLogRecordFormatsDecodedRecord(t *testing.T) {
	got := formatAgentHookLogRecord(map[string]json.RawMessage{
		"event_id":   json.RawMessage(`1`),
		"event_type": json.RawMessage(`"issue.created"`),
		"stderr":     json.RawMessage(`"first line\nsecond line"`),
	})

	assert.Equal(t, `OK daemon_log event_id=1 event_type=issue.created stderr="first line\nsecond line"`, got)
}

// TestDaemonLogs_Hooks_Tail_RotatedOnlyWaitsForActive guards the
// awaitActiveFile contract that --tail will not latch onto a rotated
// runs.jsonl.N when the active runs.jsonl is missing. Before the fix,
// the tail loop would early-return with the smallest-numbered rotated
// file and never observe future writes to runs.jsonl.
func TestDaemonLogs_Hooks_Tail_RotatedOnlyWaitsForActive(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	// Only a rotated file exists at startup.
	writeHookLog(t, dir, "runs.jsonl.1", map[string]any{"event_id": 99, "result": "ok"})

	resetFlags(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	appendHookLogAsync(filepath.Join(dir, "runs.jsonl"), 300*time.Millisecond,
		map[string]any{"event_id": 7, "result": "ok"})

	out, _, _ := executeRootCapture(t, ctx, "daemon", "logs", "--hooks", "--tail")
	if !strings.Contains(out, `"event_id":7`) {
		t.Fatalf("tail must follow runs.jsonl after it appears: %q", out)
	}
}

// TestEmitNewLines_PartialTrailingLine_NotConsumed pins the contract
// that emitNewLines does NOT advance the caller's offset across a
// partial trailing line. Before the fix, every scanned record advanced
// `read` by len(line)+1, which over-counted the unflushed mid-line by
// 1 byte and caused later ticks to miss content.
func TestEmitNewLines_PartialTrailingLine_NotConsumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	first := `{"event_id":1,"result":"ok"}` + "\n"
	partial := `{"event_id":2`
	if err := os.WriteFile(path, []byte(first+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &hookLogFilter{hookIndex: -1}
	var stdout, stderr bytes.Buffer
	n, err := emitNewLines(path, 0, &stdout, &stderr, f)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(first)) {
		t.Fatalf("read=%d, want %d (partial line must not advance offset)", n, len(first))
	}
	if !strings.Contains(stdout.String(), `"event_id":1`) {
		t.Fatalf("first line should print, got %q", stdout.String())
	}
	if strings.Contains(stdout.String(), `"event_id":2`) {
		t.Fatalf("partial line must not print, got %q", stdout.String())
	}

	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: test-controlled temp path
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(`,"result":"ok"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = fh.Close()

	stdout.Reset()
	stderr.Reset()
	n2, err := emitNewLines(path, n, &stdout, &stderr, f)
	if err != nil {
		t.Fatal(err)
	}
	if n2 == 0 {
		t.Fatalf("second tick should consume the now-completed line, got n2=%d", n2)
	}
	if !strings.Contains(stdout.String(), `"event_id":2`) {
		t.Fatalf("second line should print after completion: %q", stdout.String())
	}
}

// TestFollowActive_NoMark_EmitsExistingContent pins the contract that
// when runHookLogOnce did not see runs.jsonl (mark.set=false), follow
// emits everything currently in the file. This covers the case where
// the active file appears between one-shot and follow.
func TestFollowActive_NoMark_EmitsExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	if err := os.WriteFile(path, []byte(`{"event_id":42,"result":"ok"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	f := &hookLogFilter{hookIndex: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := followActive(ctx, &buf, io.Discard, path, activeMark{}, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"event_id":42`) {
		t.Fatalf("unset mark should emit existing content: %q", buf.String())
	}
}

// TestFollowActive_MarkAtSize_DoesNotReEmit pins the contract that
// when runHookLogOnce already consumed the file up to mark.size,
// follow resumes there and does NOT re-print prior content.
func TestFollowActive_MarkAtSize_DoesNotReEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	initial := []byte(`{"event_id":1,"result":"ok"}` + "\n")
	if err := os.WriteFile(path, initial, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	f := &hookLogFilter{hookIndex: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	mark := activeMark{set: true, info: info, size: info.Size()}
	if err := followActive(ctx, &buf, io.Discard, path, mark, f); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"event_id":1`) {
		t.Fatalf("mark at size should suppress re-emission: %q", buf.String())
	}
}

// TestRunHookLogOnce_Mark_ReportsActiveFileSize verifies that the
// one-shot pass reports a set mark with the size of runs.jsonl after
// it was read, so tail can resume at the exact byte offset.
func TestRunHookLogOnce_Mark_ReportsActiveFileSize(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	contents := `{"event_id":1,"result":"ok"}` + "\n"
	path := filepath.Join(dir, "runs.jsonl")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	mark, err := runHookLogOnce(&stdout, &stderr, 100, &hookLogFilter{hookIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !mark.set {
		t.Fatal("mark should be set when runs.jsonl was in the snapshot")
	}
	if mark.size != int64(len(contents)) {
		t.Fatalf("mark.size = %d, want %d", mark.size, len(contents))
	}
}

// TestRunHookLogOnce_Mark_UnsetWhenActiveAbsent verifies that the
// one-shot mark stays unset when runs.jsonl is absent — so follow
// later starts at offset 0 once the file appears.
func TestRunHookLogOnce_Mark_UnsetWhenActiveAbsent(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	// Only a rotated file exists.
	writeHookLog(t, dir, "runs.jsonl.1", map[string]any{"event_id": 99, "result": "ok"})
	var stdout, stderr bytes.Buffer
	mark, err := runHookLogOnce(&stdout, &stderr, 100, &hookLogFilter{hookIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	if mark.set {
		t.Fatal("mark must stay unset when runs.jsonl wasn't in snapshot")
	}
}

func TestDaemonLogs_Hooks_Tail_PicksUpNewLines(t *testing.T) {
	_, dir, _ := setupHooksDir(t)
	path := filepath.Join(dir, "runs.jsonl")
	writeHookLog(t, dir, "runs.jsonl", map[string]any{"event_id": 1, "result": "ok"})

	resetFlags(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	appendHookLogAsync(path, 200*time.Millisecond,
		map[string]any{"event_id": 2, "result": "ok"})

	out, _, _ := executeRootCapture(t, ctx, "daemon", "logs", "--hooks", "--tail")
	if !strings.Contains(out, `"event_id":1`) || !strings.Contains(out, `"event_id":2`) {
		t.Fatalf("tail should print initial + appended: %q", out)
	}
}

// TestDaemonLogs_RejectsNonPositiveLimit covers hammer-test
// finding #7 part 1: --limit -1 / --limit 0 used to be silently
// treated as "no limit", contradicting the help text. Reject with
// kindValidation so the user sees what actually happened.
func TestDaemonLogs_RejectsNonPositiveLimit(t *testing.T) {
	for _, lim := range []string{"0", "-1"} {
		resetFlags(t)
		_, _, err := executeRootCapture(t, context.Background(),
			"daemon", "logs", "--hooks", "--limit", lim)
		require.Errorf(t, err, "--limit %s should reject", lim)
		var ce *cliError
		require.True(t, errors.As(err, &ce))
		assert.Equal(t, ExitValidation, ce.ExitCode)
	}
}

// TestDaemonLogs_RejectsHookIndexBelowMinusOne covers hammer-test
// finding #7 part 2: --hook-index -2 used to be silently accepted,
// contradicting the help text where -1 means "all". Anything below
// -1 is meaningless; reject loudly.
func TestDaemonLogs_RejectsHookIndexBelowMinusOne(t *testing.T) {
	resetFlags(t)
	_, _, err := executeRootCapture(t, context.Background(),
		"daemon", "logs", "--hooks", "--hook-index", "-2")
	require.Error(t, err)
	var ce *cliError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, ExitValidation, ce.ExitCode)
}
