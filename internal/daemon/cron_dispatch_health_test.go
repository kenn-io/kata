package daemon

import (
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func TestCronRecorderLastNonIdleSurvivesIdleAndPrunes(t *testing.T) {
	r := NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-private-detail"), now)
	require.NoError(t, err)
	_, err = r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, now.Add(time.Minute))
	require.NoError(t, err)
	last, nonIdle := r.Last(1, "job")
	require.Equal(t, cron.DispatchIdle, last.Reason)
	require.Equal(t, cron.DispatchError, nonIdle.Reason)
	require.NotContains(t, nonIdle.Detail, "local-private-detail")
	nonIdle.Reason = cron.DispatchIdle
	_, nonIdle = r.Last(1, "job")
	require.Equal(t, cron.DispatchError, nonIdle.Reason, "lookups return copies")
	last, nonIdle = r.Last(2, "job")
	require.Nil(t, last)
	require.Nil(t, nonIdle)
	r.FinishCycle(now, 0, true, false)
	r.FinishCycle(now.Add(time.Minute), 0, true, false)
	last, nonIdle = r.Last(1, "job")
	require.Nil(t, last)
	require.Nil(t, nonIdle)
}

func TestCronRecorderRetriesUnchangedLogFailure(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
	r := NewCronDispatchRecorder(filepath.Join(blocked, "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candidate := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	out := db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}
	_, err := r.Record(t.Context(), candidate, out, nil, now)
	require.Error(t, err)
	r.FinishCycle(now, 0, true, false)
	require.NoError(t, os.Remove(blocked))
	changed, err := r.Record(t.Context(), candidate, out, nil, now.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, changed, "retrying the log must not repeat decision alerts")
	require.False(t, r.Health().LogErrorPresent)
	require.EqualValues(t, 1, r.Health().LogErrorsSinceStart)
	raw, err := os.ReadFile(r.Health().LogPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"reason":"idle"`)
}

func TestCronRecorderEnumerationFreshnessAndLowerBound(t *testing.T) {
	r := NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := r.Record(t.Context(), db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}, db.DispatchRecurringCronOut{Reason: cron.DispatchSkipped, Skipped: cron.RecurringSkip{Count: 1000, Capped: true}}, nil, now)
	require.NoError(t, err)
	r.FinishCycle(now, 0, false, false) // one successfully listed page
	r.FinishCycle(now.Add(time.Minute), 0, false, true)
	raw, err := json.Marshal(r.Health())
	require.NoError(t, err)
	var health map[string]any
	require.NoError(t, json.Unmarshal(raw, &health))
	require.Equal(t, now.Format(time.RFC3339), health["last_enumeration_at"])
	require.EqualValues(t, 1, health["enumeration_errors_since_start"])
	require.Equal(t, true, health["skipped_count_is_lower_bound"])
	require.Equal(t, 1, r.Health().Jobs, "failed enumeration retains observations")
	r.FinishCycle(now.Add(2*time.Minute), 0, true, false)
	raw, err = json.Marshal(r.Health())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &health))
	require.Equal(t, true, health["skipped_count_is_lower_bound"], "lifetime count remains approximate after pruning")
	require.Zero(t, r.Health().Jobs)
}
