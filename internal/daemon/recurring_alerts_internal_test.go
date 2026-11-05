package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

type recurringAlertStore struct {
	db.Storage
	calls        int
	listingError error
}

func (s *recurringAlertStore) EnsureScheduledReachedFloor(_ context.Context, now time.Time) (time.Time, error) {
	return now, nil
}
func (s *recurringAlertStore) ListDueNotificationIssueIDs(context.Context, time.Time) ([]int64, error) {
	return nil, s.listingError
}
func (s *recurringAlertStore) ListRecurringCronJobs(context.Context, string, int) ([]db.RecurringCronCandidate, error) {
	return []db.RecurringCronCandidate{{ProjectID: 1, JobUID: "job"}}, nil
}
func (s *recurringAlertStore) DispatchRecurringCron(context.Context, db.DispatchRecurringCronIn) (db.DispatchRecurringCronOut, error) {
	s.calls++
	detail := "first failure"
	if s.calls > 2 {
		detail = "changed failure"
	}
	return db.DispatchRecurringCronOut{Reason: cron.DispatchError}, errors.New(detail)
}

func TestRecurringAlertsPreserveErrorsAndSuppressOnlyRepeats(t *testing.T) {
	store := &recurringAlertStore{}
	r := NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	s := NewDueNotificationSweeper(store, EventPublisher{}, "UTC")
	s.CronRecorder = r
	// Ordinary callers still receive identical recurring failures every time.
	require.ErrorContains(t, s.RunOnce(t.Context(), time.Now()), "first failure")
	result := s.runOnce(t.Context(), time.Now())
	require.ErrorContains(t, result.err, "first failure")
	require.NoError(t, result.alert)
	result = s.runOnce(t.Context(), time.Now())
	require.ErrorContains(t, result.err, "changed failure")
	require.ErrorContains(t, result.alert, "changed failure")
	store.listingError = errors.New("ordinary listing failure")
	result = s.runOnce(t.Context(), time.Now())
	require.ErrorContains(t, result.err, "ordinary listing failure")
	require.ErrorContains(t, result.alert, "ordinary listing failure")
	require.NotContains(t, result.alert.Error(), "changed failure")
}

func TestRecurringOnErrorOncePerChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &recurringAlertStore{}
		r := NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		s := NewDueNotificationSweeper(store, EventPublisher{}, "UTC")
		s.CronRecorder = r
		s.Interval = time.Millisecond
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		alerts := 0
		s.OnError = func(error) {
			alerts++
			if alerts == 2 {
				cancel()
			}
		}
		require.ErrorIs(t, s.Run(ctx), context.Canceled)
		require.Equal(t, 2, alerts)
		require.GreaterOrEqual(t, store.calls, 3)
	})
}

// Enumeration, rather than a successful dispatch, determines which retained
// observations survive a completed cycle, including the final shutdown page.
type canceledRecurringStore struct {
	recurringAlertStore
	candidates []db.RecurringCronCandidate
	cancel     context.CancelFunc
}

func (s *canceledRecurringStore) ListRecurringCronJobs(context.Context, string, int) ([]db.RecurringCronCandidate, error) {
	return s.candidates, nil
}

func (s *canceledRecurringStore) DispatchRecurringCron(ctx context.Context, _ db.DispatchRecurringCronIn) (db.DispatchRecurringCronOut, error) {
	s.cancel()
	return db.DispatchRecurringCronOut{}, ctx.Err()
}

func TestCanceledSweepRetainsEnumeratedObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatch.jsonl")
	r := NewCronDispatchRecorder(path)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	candidate := db.RecurringCronCandidate{ProjectID: 1, JobUID: "retained", ExpectedEventUID: "revision"}
	absent := db.RecurringCronCandidate{ProjectID: 1, JobUID: "absent"}
	out := db.DispatchRecurringCronOut{Reason: cron.DispatchDelivered, Delivered: 2, Skipped: cron.RecurringSkip{Count: 3, Capped: true}}
	for _, c := range []db.RecurringCronCandidate{candidate, absent} {
		_, err := r.Record(t.Context(), c, out, nil, now)
		require.NoError(t, err)
	}
	r.FinishCycle(now, 0, true, false)
	last, nonIdle := r.Last(1, candidate.JobUID)
	before := r.Health()
	history, err := os.ReadFile(path) //nolint:gosec // G304: TempDir fixture
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := &canceledRecurringStore{candidates: []db.RecurringCronCandidate{candidate, {ProjectID: 1, JobUID: "unobserved"}}, cancel: cancel}
	s := NewDueNotificationSweeper(store, EventPublisher{}, "UTC")
	s.CronRecorder = r
	result := s.runOnce(ctx, now.Add(time.Minute))
	require.ErrorIs(t, result.err, context.Canceled)
	require.NoError(t, result.alert, "shutdown failures must not alert")
	gotLast, gotNonIdle := r.Last(1, candidate.JobUID)
	require.NotNil(t, gotLast, "enumerated candidate must retain its prior observation")
	require.Equal(t, last, gotLast)
	require.Equal(t, nonIdle, gotNonIdle)
	h := r.Health()
	require.Equal(t, 1, h.Jobs, "absent candidate pruned; canceled new candidate not invented")
	require.Equal(t, map[string]int{"delivered": 1}, h.ByReason)
	require.Equal(t, before.DeliveredSinceStart, h.DeliveredSinceStart)
	require.Equal(t, before.SkippedSinceStart, h.SkippedSinceStart)
	require.True(t, h.SkippedCountIsLowerBound)
	require.Zero(t, h.ErrorsSinceStart)
	require.Zero(t, h.LogErrorsSinceStart)
	require.False(t, h.LogErrorPresent)
	require.Zero(t, h.EnumerationErrorsSinceStart)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: TempDir fixture
	require.NoError(t, err)
	require.Equal(t, history, raw, "canceled attempts write no history")
	// A later complete enumeration that omits the candidate still prunes it.
	store.candidates = nil
	require.NoError(t, s.RunOnce(t.Context(), now.Add(2*time.Minute)))
	require.Zero(t, r.Health().Jobs)
	gotLast, gotNonIdle = r.Last(1, candidate.JobUID)
	require.Nil(t, gotLast)
	require.Nil(t, gotNonIdle)
}

// last_sweep_ms covers date notifications as well as recurring dispatch.
type slowDateNotificationStore struct {
	canceledRecurringStore
	wait func()
}

func (s *slowDateNotificationStore) ListDueNotificationIssueIDs(context.Context, time.Time) ([]int64, error) {
	s.wait()
	return nil, nil
}

func TestCronHealthTimesWholeSweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewCronDispatchRecorder(filepath.Join(t.TempDir(), "dispatch.jsonl"))
		t.Cleanup(func() { require.NoError(t, r.Close()) })
		store := &slowDateNotificationStore{wait: func() { time.Sleep(2 * time.Second) }}
		s := NewDueNotificationSweeper(store, EventPublisher{}, "UTC")
		s.CronRecorder = r
		require.NoError(t, s.RunOnce(t.Context(), time.Now()))
		require.EqualValues(t, 2000, r.Health().LastSweepMS)
	})
}
