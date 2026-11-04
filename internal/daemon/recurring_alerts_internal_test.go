package daemon

import (
	"context"
	"errors"
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
