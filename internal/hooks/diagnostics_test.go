package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDoctorHookDiagnosticsUseActiveSnapshot(t *testing.T) {
	workdir := t.TempDir()
	executable, err := os.Executable()
	require.NoError(t, err)
	h := ResolvedHook{Index: 0, Event: "*", Command: executable, WorkingDir: workdir, Args: []string{"secret-token"}, UserEnv: []string{"SECRET=secret-token"}}
	d, _, _ := mustNewDispatcher(t, []ResolvedHook{h}, defaultConfig())
	d.dropped.Store(4)
	got := d.Diagnostics()
	require.True(t, got.Available)
	require.Len(t, got.Hooks, 1)
	require.True(t, got.Hooks[0].ExecutableAvailable)
	require.True(t, got.Hooks[0].WorkingDirectoryAvailable)
	require.EqualValues(t, 4, got.Dropped)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret-token")
	h.Command = "missing-doctor-hook"
	h.WorkingDir = filepath.Join(workdir, "missing")
	d.snapshot.Store(&Snapshot{Hooks: []ResolvedHook{h}})
	got = d.Diagnostics()
	require.False(t, got.Hooks[0].ExecutableAvailable)
	require.False(t, got.Hooks[0].WorkingDirectoryAvailable)
}

func TestDoctorHookHistoryCapKeepsNewestFailures(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := fmt.Sprintf("{\"kata_hook_runs_version\":1,\"ended_at\":%q,\"result\":\"ok\",\"exit_code\":0}\n", now.Add(-8*24*time.Hour).Format(time.RFC3339Nano))
	recent := fmt.Sprintf("{\"kata_hook_runs_version\":1,\"ended_at\":%q,\"result\":\"ok\",\"exit_code\":7}\n", now.Add(-time.Minute).Format(time.RFC3339Nano))
	active := filepath.Join(t.TempDir(), "runs.jsonl")
	d := &Dispatcher{appender: &runsAppender{path: active}, deps: DispatcherDeps{Now: func() time.Time { return now }}}
	require.NoError(t, os.WriteFile(active, []byte(strings.Repeat(old, (9<<20)/len(old))+recent), 0600))
	h := d.Diagnostics()
	require.False(t, h.HistoryIncomplete, "a scan limit is not a history read error")
	require.True(t, h.HistoryTruncated)
	require.Equal(t, 1, h.RecentRuns)
	require.Equal(t, 1, h.RecentFailures, "the read budget must prioritize the newest retained runs")
}

func TestDoctorHookRetainedHistoryCountsNonzeroAndRotatedFailures(t *testing.T) {
	d, _, path := mustNewDispatcher(t, nil, defaultConfig())
	now := time.Now().UTC()
	line := func(end time.Time, result string, exit int) string {
		return fmt.Sprintf(`{"kata_hook_runs_version":1,"ended_at":%q,"result":%q,"exit_code":%d,"spawn_error":"secret-token"}`+"\n", end.Format(time.RFC3339Nano), result, exit)
	}
	require.NoError(t, os.WriteFile(path, []byte(line(now, "ok", 0)+line(now, "ok", 7)), 0600))
	require.NoError(t, os.WriteFile(path+".1", []byte(line(now, "spawn_failed", -1)+line(now.Add(-8*24*time.Hour), "timed_out", -1)), 0600))
	got := d.Diagnostics()
	require.Equal(t, 3, got.RecentRuns)
	require.Equal(t, 2, got.RecentFailures)
	require.False(t, got.HistoryIncomplete)
	require.NoError(t, os.WriteFile(path+".2", []byte("invalid secret-token\n"), 0600))
	got = d.Diagnostics()
	require.True(t, got.HistoryIncomplete)
	data, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(data), "secret-token")
}

func TestDoctorHookHistoryMissingAndCapped(t *testing.T) {
	d, _, path := mustNewDispatcher(t, nil, defaultConfig())
	require.NoError(t, d.appender.Close())
	require.NoError(t, os.Remove(path))
	require.True(t, d.Diagnostics().HistoryIncomplete)
	require.NoError(t, os.WriteFile(path, make([]byte, 9<<20), 0600))
	require.True(t, d.Diagnostics().HistoryIncomplete)
}

func TestDoctorHookHistoryExactlyAtBudget(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	base := fmt.Sprintf("{\"kata_hook_runs_version\":1,\"ended_at\":%q,\"result\":\"ok\",\"exit_code\":0}", now.Format(time.RFC3339Nano))
	line := base + strings.Repeat(" ", 1023-len(base)) + "\n"
	empty := ""
	for _, tc := range []struct {
		name      string
		rotated   *string
		truncated bool
	}{
		{name: "no remaining file"},
		{name: "empty remaining file", rotated: &empty},
		{name: "unread remaining records", rotated: &line, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runs.jsonl")
			d := &Dispatcher{appender: &runsAppender{path: path, keep: 1}, deps: DispatcherDeps{Now: func() time.Time { return now }}}
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat(line, 8192)), 0600))
			if tc.rotated != nil {
				require.NoError(t, os.WriteFile(path+".1", []byte(*tc.rotated), 0600))
			}
			got := d.Diagnostics()
			require.Equal(t, 8192, got.RecentRuns)
			require.Zero(t, got.RecentFailures)
			require.False(t, got.HistoryIncomplete)
			require.Equal(t, tc.truncated, got.HistoryTruncated)
		})
	}
}

func TestDoctorHookHistoryFileLimitOnlyMarksOmittedRecords(t *testing.T) {
	now := time.Now().UTC()
	line := fmt.Sprintf(`{"kata_hook_runs_version":1,"ended_at":%q,"result":"ok","exit_code":0}`+"\n", now.Format(time.RFC3339Nano))
	for _, tc := range []struct {
		name      string
		rotated   *string
		truncated bool
	}{
		{name: "no rotated files"},
		{name: "empty older file", rotated: new("")},
		{name: "older records beyond a gap", rotated: new(line), truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "runs.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(line), 0600))
			if tc.rotated != nil {
				require.NoError(t, os.WriteFile(path+".100", []byte(*tc.rotated), 0600))
			}
			d := &Dispatcher{appender: &runsAppender{path: path, keep: 100}, deps: DispatcherDeps{Now: func() time.Time { return now }}}
			got := d.Diagnostics()
			require.Equal(t, tc.truncated, got.HistoryTruncated)
			require.False(t, got.HistoryIncomplete)
			require.Equal(t, 1, got.RecentRuns, "records beyond the file limit are not scanned")
		})
	}
}

func TestDoctorHookHistoryRequiresExactFieldNames(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	d := &Dispatcher{appender: &runsAppender{path: path, keep: 1}, deps: DispatcherDeps{Now: func() time.Time { return now }}}
	line := `{"KATA_HOOK_RUNS_VERSION":1,"ENDED_AT":"2026-09-01T12:00:00Z","RESULT":"ok","EXIT_CODE":0}`
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0600))

	got := d.Diagnostics()
	require.Zero(t, got.RecentRuns, "history records use the exact field names written by the hook runner")
	require.True(t, got.HistoryIncomplete)
}
