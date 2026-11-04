package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestDaemonCronLogsChronologyAndFilters(t *testing.T) {
	_, _, hash := setupHooksDir(t)
	path, err := config.CronDispatchLogPath(hash)
	require.NoError(t, err)
	dir := filepath.Dir(path)
	writeRuns(t, dir, map[string][]map[string]any{
		"dispatch.jsonl.2": {{"job_uid": "job-a", "reason": "idle", "n": 1}},
		"dispatch.jsonl.1": {{"job_uid": "job-b", "reason": "delivered", "n": 2}},
		"dispatch.jsonl":   {{"job_uid": "job-a", "reason": "parent_closed", "n": 3}, {"job_uid": "job-a", "reason": "parent_closed", "n": 4}},
	})
	resetFlags(t)
	out, _, err := executeRootCapture(t, t.Context(), "daemon", "logs", "--cron", "--job-uid", "job-a", "--reason", "parent_closed", "--limit", "1")
	require.NoError(t, err)
	require.Contains(t, out, `"n":4`)
	require.NotContains(t, out, `"n":3`)
	resetFlags(t)
	out, _, err = executeRootCapture(t, t.Context(), "daemon", "logs", "--cron", "--limit", "2")
	require.NoError(t, err)
	require.Contains(t, out, `"n":3`)
	require.Contains(t, out, `"n":4`)
	require.NotContains(t, out, `"n":2`)
}

func TestDaemonCronLogsRejectsIncompatibleSelectors(t *testing.T) {
	setupHooksDir(t)
	for _, flags := range [][]string{
		{"--hooks", "--cron"}, {"--cron", "--failed-only"}, {"--cron", "--event-type", "issue.created"},
		{"--cron", "--hook-index", "0"}, {"--hooks", "--job-uid", "job"}, {"--hooks", "--reason", "idle"},
	} {
		resetFlags(t)
		_, _, err := executeRootCapture(t, t.Context(), append([]string{"daemon", "logs"}, flags...)...)
		require.Error(t, err, flags)
	}
}

func TestDaemonCronLogsTail(t *testing.T) {
	_, _, hash := setupHooksDir(t)
	path, err := config.CronDispatchLogPath(hash)
	require.NoError(t, err)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		// Follow waits for the active file, then emits newly appended records.
		go func() {
			time.Sleep(300 * time.Millisecond)
			writeHookLog(t, filepath.Dir(path), "dispatch.jsonl", map[string]any{"job_uid": "job-a", "reason": "idle"})
		}()
		resetFlags(t)
		out, _, err := executeRootCapture(t, ctx, "daemon", "logs", "--cron", "--tail", "--reason", "idle")
		require.NoError(t, err)
		require.Contains(t, out, `"job_uid":"job-a"`)
	})
}

// A snapshot must leave an incomplete JSONL record for the following poll.
func TestCronLogSnapshotRetainsPartialRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n{\"n\":2"), 0o600))
	var out bytes.Buffer
	f := &cronLogFilter{}
	mark, err := runDaemonLogOnce(t.Context(), path, &out, io.Discard, 100, f)
	require.NoError(t, err)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: test-controlled temp path
	require.NoError(t, err)
	_, err = fh.WriteString("}\n")
	require.NoError(t, err)
	require.NoError(t, fh.Close())
	st := &followState{prevInfo: mark.info, prevSize: mark.size}
	require.NoError(t, tailTick(t.Context(), path, &out, io.Discard, st, f))
	require.Equal(t, "{\"n\":1}\n{\"n\":2}\n", out.String())
}

// Records appended to the old file just before rotation still belong to tail.
func TestCronLogTailDrainsRotatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n"), 0o600))
	var out bytes.Buffer
	f := &cronLogFilter{}
	mark, err := runDaemonLogOnce(t.Context(), path, &out, io.Discard, 100, f)
	require.NoError(t, err)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = fh.WriteString("{\"n\":2}\n")
	require.NoError(t, err)
	require.NoError(t, fh.Close())
	require.NoError(t, os.Rename(path, path+".1"))
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":3}\n"), 0o600))
	st := &followState{prevInfo: mark.info, prevSize: mark.size}
	require.NoError(t, tailTick(t.Context(), path, &out, io.Discard, st, f))
	require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n", out.String())
}

// The pinned replacement can itself rotate before draining begins. Its open
// handle will be read next, so the rotation drain must stop before that inode.
func TestCronLogDrainStopsAtPinnedReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":1}\n"), 0o600))
	old, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".2"))
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":2}\n"), 0o600))
	pinned, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".1"))
	require.NoError(t, os.WriteFile(path, []byte("{\"n\":3}\n"), 0o600))
	var out bytes.Buffer
	require.NoError(t, drainRotatedLog(t.Context(), path, &out, io.Discard, &followState{prevInfo: old}, pinned, &cronLogFilter{}))
	require.Equal(t, "{\"n\":1}\n", out.String())
}

func TestCronLogDrainResumesFromOldestRetainedAfterCursorEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	evicted := path + ".4"
	require.NoError(t, os.WriteFile(evicted, []byte("{\"n\":0}\n"), 0o600))
	old, err := os.Stat(evicted)
	require.NoError(t, err)
	require.NoError(t, os.Remove(evicted))
	for name, n := range map[string]int{path + ".3": 1, path + ".2": 2, path + ".1": 3, path: 4} {
		require.NoError(t, os.WriteFile(name, []byte(fmt.Sprintf("{\"n\":%d}\n", n)), 0o600))
	}
	pinned, err := os.Stat(path)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, drainRotatedLog(t.Context(), path, &out, io.Discard, &followState{prevInfo: old}, pinned, &cronLogFilter{}))
	require.Equal(t, "{\"n\":1}\n{\"n\":2}\n{\"n\":3}\n", out.String())
}

type cronRotationWriter struct {
	bytes.Buffer
	rotate func()
}

func (w *cronRotationWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.rotate != nil {
		rotate := w.rotate
		w.rotate = nil
		rotate()
	}
	return n, err
}

// Rotation during emission must not change the file identity associated with
// a generation that the drain has yet to consume.
func TestCronLogDrainPinsGenerationsBeforeEmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	for name, n := range map[string]int{path + ".2": 1, path + ".1": 2, path: 3} {
		require.NoError(t, os.WriteFile(name, []byte(fmt.Sprintf("{\"n\":%d}\n", n)), 0o600))
	}
	old, err := os.Stat(path + ".2")
	require.NoError(t, err)
	pinned, err := os.Stat(path)
	require.NoError(t, err)
	out := &cronRotationWriter{rotate: func() {
		require.NoError(t, os.Rename(path+".2", path+".3"))
		require.NoError(t, os.Rename(path+".1", path+".2"))
		require.NoError(t, os.Rename(path, path+".1"))
		require.NoError(t, os.WriteFile(path, nil, 0o600))
	}}
	require.NoError(t, drainRotatedLog(t.Context(), path, out, io.Discard, &followState{prevInfo: old}, pinned, &cronLogFilter{}))
	require.Equal(t, "{\"n\":1}\n{\"n\":2}\n", out.String())
}

// A writer may leave active missing after a failed reopen. Tail must yield
// until its next poll rather than spin until the command is canceled.
func TestCronLogPinMissingActiveYields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		files, err := pinRotatedLogs(ctx, filepath.Join(t.TempDir(), "dispatch.jsonl"))
		require.Empty(t, files)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}
