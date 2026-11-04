package daemon_test

import (
	"context"
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
	require.True(t, record(db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil))
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
	require.Equal(t, 3, strings.Count(string(raw), "\n"), "steady states must not flood log")
	require.Contains(t, string(raw), `"kind":"skipped"`)

	// Failed or paginated enumeration cannot prune previous observations.
	r.FinishCycle(now.Add(time.Minute), 0, false, true)
	require.Equal(t, 1, r.Health().Jobs)
	r.FinishCycle(now.Add(2*time.Minute), 0, false, false)
	require.Equal(t, 1, r.Health().Jobs)
	r.FinishCycle(now.Add(3*time.Minute), 0, true, false)
	require.Zero(t, r.Health().Jobs)
	require.Nil(t, recorderLast(r, c.ProjectID, c.JobUID))
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

func TestCronRecorderProjectKeysAndLogFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron", "dispatch.jsonl")
	r := daemon.NewCronDispatchRecorder(path)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New("private failure"), time.Now())
	require.NoError(t, err)
	require.Nil(t, recorderLast(r, 2, "job"))
	require.NotContains(t, recorderLast(r, 1, "job").Detail, "private failure")
	// Each response is a copy; callers cannot alter a retained observation.
	recorderLast(r, 1, "job").Reason = cron.DispatchIdle
	require.Equal(t, cron.DispatchError, recorderLast(r, 1, "job").Reason)
}

// Filesystem failures are local diagnostics. A later changed decision retries
// opening; steady states still obey the documented deduplication contract.
func TestCronRecorderLogFailureHealthAndRecovery(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
	r := daemon.NewCronDispatchRecorder(filepath.Join(blocked, "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	c := db.RecurringCronCandidate{ProjectID: 1, JobUID: "job"}
	_, err := r.Record(t.Context(), c, db.DispatchRecurringCronOut{Reason: cron.DispatchIdle}, nil, time.Now())
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
