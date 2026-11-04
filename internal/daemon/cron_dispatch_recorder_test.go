package daemon_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCronRecorderCommittedCountsDedupeAndPruning(t *testing.T) {
	r := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	require.Nil(t, r.Health(), "no health before first sweep")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := db.RecurringCronCandidate{ProjectUID: "project", JobUID: "job", ExpectedEventUID: "revision"}
	record := func(out db.DispatchRecurringCronOut, err error) bool {
		t.Helper()
		changed, logErr := r.Record(t.Context(), c, out, err, now)
		require.NoError(t, logErr)
		return changed
	}
	require.False(t, record(db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil))
	require.False(t, record(db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil))
	record(db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 2, Skipped: cron.RecurringSkip{Count: 1000, Capped: true}}, nil)
	require.True(t, record(db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-sensitive-detail")))
	require.False(t, record(db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-sensitive-detail")))
	r.FinishCycle(now, time.Millisecond, true, false)
	h := r.Health()
	require.Equal(t, 1, h.Jobs)
	require.EqualValues(t, 2, h.DeliveredSinceStart)
	require.EqualValues(t, 1000, h.SkippedSinceStart)
	require.EqualValues(t, 2, h.ErrorsSinceStart)
	require.Equal(t, 1, h.ByReason["error"])
	last := recorderLast(r, c.ProjectID, c.JobUID)
	require.NotNil(t, last)
	require.NotContains(t, last.Detail, "local-sensitive-detail")
	raw, err := os.ReadFile(h.LogPath) //nolint:gosec // G304: test-controlled path under t.TempDir()
	require.NoError(t, err)
	require.Contains(t, string(raw), "local-sensitive-detail")
	require.Equal(t, 2, strings.Count(string(raw), "\n"), "steady states must not flood log")
	require.Contains(t, string(raw), `"kind":"skipped"`)

	// Failed or paginated enumeration cannot prune previous observations.
	r.FinishCycle(now.Add(time.Minute), 0, false, true)
	require.Equal(t, 1, r.Health().Jobs)
	r.FinishCycle(now.Add(2*time.Minute), 0, false, false)
	require.Equal(t, 1, r.Health().Jobs)
	r.FinishCycle(now.Add(3*time.Minute), 0, true, false)
	require.Zero(t, r.Health().Jobs)
	last, nonIdle := r.Last(c.ProjectID, c.JobUID)
	require.Nil(t, last)
	require.Nil(t, nonIdle)
}

type failingRecurringStore struct {
	db.Storage
	calls int
}

func (s *failingRecurringStore) DispatchRecurringCron(ctx context.Context, in db.DispatchRecurringCronIn) (db.DispatchRecurringCronOut, error) {
	s.calls++
	if s.calls == 2 {
		return db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("second batch failed")
	}
	return s.Storage.DispatchRecurringCron(ctx, in)
}

func TestCronSweeperRetainsCommittedBatchBeforeLaterError(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	p, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	parent, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Recurring commitment", Author: "operator"})
	require.NoError(t, err)
	job, _, err := env.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: p.ID, Name: "Review", Actor: "operator", Definition: cron.JobDefinition{Version: 1, Kind: "job", Enabled: true, Trigger: cron.Trigger{Kind: "interval", IntervalSeconds: 60}, Action: cron.Action{Kind: "notify", Recipient: "operator"}, Issue: &cron.IssuePolicy{Kind: "per-run", ParentUID: parent.UID, Title: "Review"}, Overlap: "allow", Catchup: "all"}})
	require.NoError(t, err)
	r := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	sink := &captureHookSink{}
	s := daemon.NewDueNotificationSweeper(&failingRecurringStore{Storage: env.DB}, daemon.NewEventPublisher(daemon.NewEventBroadcaster(), sink), "UTC")
	s.CronRecorder = r
	require.ErrorContains(t, s.RunOnce(ctx, time.Now().Add(61*time.Minute)), "second batch failed")
	require.EqualValues(t, 50, r.Health().DeliveredSinceStart)
	require.EqualValues(t, 1, r.Health().ErrorsSinceStart)
	require.Len(t, sink.events, 51)
	require.Equal(t, cron.DispatchError, recorderLast(r, p.ID, job.UID).Reason)
}

// Cancellation after commit cannot erase a batch's delivered counts. A failed
// attempt caused by shutdown should not create a diagnostic error instead.
func TestCronRecorderKeepsCommitAfterCallerCancellation(t *testing.T) {
	r := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(ctx, c, db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 2}, nil, time.Now())
	require.NoError(t, err)
	_, err = r.Record(ctx, c, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, context.Canceled, time.Now())
	require.NoError(t, err)
	r.FinishCycle(time.Now(), 0, true, false)
	require.EqualValues(t, 2, r.Health().DeliveredSinceStart)
	require.Zero(t, r.Health().ErrorsSinceStart)
	require.Equal(t, cron.DispatchDelivered, recorderLast(r, 1, "job").Reason)
}

func recorderLast(r *daemon.CronDispatchRecorder, projectID int64, jobUID string) *api.CronDispatchDecision {
	last, _ := r.Last(projectID, jobUID)
	return last
}

func TestCronRecorderLastNonIdleSurvivesIdle(t *testing.T) {
	r := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "cron", "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("local-private-detail"), now)
	require.NoError(t, err)
	errorLast, _ := r.Last(1, "job")
	require.NotContains(t, errorLast.Detail, "local-private-detail")
	errorLast.Reason = cron.DispatchIdle
	errorLast, _ = r.Last(1, "job")
	require.Equal(t, cron.DispatchError, errorLast.Reason, "error lookups return copies")
	_, err = r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, now.Add(time.Minute))
	require.NoError(t, err)
	last, nonIdle := r.Last(1, "job")
	require.Equal(t, cron.DispatchIdle, last.Reason)
	last.Reason = cron.DispatchError
	last, _ = r.Last(1, "job")
	require.Equal(t, cron.DispatchIdle, last.Reason, "latest lookups also return copies")
	require.Equal(t, cron.DispatchError, nonIdle.Reason)
	require.NotContains(t, nonIdle.Detail, "local-private-detail")
	nonIdle.Reason = cron.DispatchIdle
	_, nonIdle = r.Last(1, "job")
	require.Equal(t, cron.DispatchError, nonIdle.Reason, "lookups return copies")
	last, nonIdle = r.Last(2, "job")
	require.Nil(t, last)
	require.Nil(t, nonIdle)
}

func TestCronRecorderLogFailureRecovery(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
	r := daemon.NewCronDispatchRecorder(filepath.Join(blocked, "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchDisabled}, nil, time.Now())
	require.Error(t, err)
	r.FinishCycle(time.Now(), 0, true, false)
	require.True(t, r.Health().LogErrorPresent)
	require.EqualValues(t, 1, r.Health().LogErrorsSinceStart)
	require.NoError(t, os.Remove(blocked))
	_, err = r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 1}, nil, time.Now())
	require.NoError(t, err)
	require.False(t, r.Health().LogErrorPresent)
	require.EqualValues(t, 1, r.Health().LogErrorsSinceStart)
}

func TestCronRecorderEnumerationFreshnessAndLowerBound(t *testing.T) {
	r := daemon.NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
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
	r.FinishCycle(now.Add(2*time.Minute), 0, true, false)
	raw, err = json.Marshal(r.Health())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &health))
	require.Equal(t, true, health["skipped_count_is_lower_bound"], "lifetime count remains approximate after pruning")
}

// A recorder restart has no previous decision to transition from. Idle jobs
// remain observable in memory without growing durable history each restart.
func TestCronRecorderInitialIdleDoesNotLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job", ExpectedEventUID: "revision"}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 2 {
		r := daemon.NewCronDispatchRecorder(path)
		_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, now)
		require.NoError(t, err)
		last, nonIdle := r.Last(1, "job")
		require.Equal(t, cron.DispatchIdle, last.Reason)
		require.Nil(t, nonIdle)
		r.FinishCycle(now, 0, true, false)
		require.Equal(t, 1, r.Health().ByReason["idle"])
		require.NoError(t, r.Close())
	}
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	r := daemon.NewCronDispatchRecorder(path)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	_, err = r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 1}, nil, now)
	require.NoError(t, err)
	_, err = r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, now)
	require.NoError(t, err)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: TempDir fixture
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(raw), "\n"), "delivery and return to idle still log")
}
