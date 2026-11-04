package main

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

// Version-1 log records remain readable when a newer daemon adds a reason.
// Filtering local history must not require that daemon's reason enum.
func TestDaemonCronLogsFiltersFutureReason(t *testing.T) {
	_, _, hash := setupHooksDir(t)
	path, err := config.CronDispatchLogPath(hash)
	require.NoError(t, err)
	writeRuns(t, filepath.Dir(path), map[string][]map[string]any{
		"dispatch.jsonl": {
			{"version": 1, "job_uid": "JOB-A", "reason": "future_reason"},
			{"version": 1, "job_uid": "JOB-B", "reason": "idle"},
		},
	})
	resetFlags(t)
	out, warnings, err := executeRootCapture(t, t.Context(), "daemon", "logs", "--cron", "--reason", "future_reason")
	require.NoError(t, err)
	require.Contains(t, out, `"reason":"future_reason"`)
	require.NotContains(t, out, `"reason":"idle"`)
	require.Empty(t, warnings)
}

func TestDaemonCronLogsChronologyAndFilters(t *testing.T) {
	_, _, hash := setupHooksDir(t)
	path, err := config.CronDispatchLogPath(hash)
	require.NoError(t, err)
	dir := filepath.Dir(path)
	writeRuns(t, dir, map[string][]map[string]any{
		"dispatch.jsonl.2": {{"job_uid": "JOB-A", "reason": "idle", "n": 1}},
		"dispatch.jsonl.1": {{"job_uid": "JOB-B", "reason": "delivered", "n": 2}},
		"dispatch.jsonl":   {{"job_uid": "JOB-A", "reason": "parent_closed", "n": 3}, {"job_uid": "JOB-A", "reason": "parent_closed", "n": 4}},
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
		var usage *cliError
		require.ErrorAs(t, err, &usage, flags)
		require.Equal(t, ExitUsage, usage.ExitCode, flags)
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
			writeHookLog(t, filepath.Dir(path), "dispatch.jsonl", map[string]any{"job_uid": "JOB-A", "reason": "idle"})
		}()
		resetFlags(t)
		out, _, err := executeRootCapture(t, ctx, "daemon", "logs", "--cron", "--tail", "--reason", "idle")
		require.NoError(t, err)
		require.Contains(t, out, `"job_uid":"JOB-A"`)
	})
}
