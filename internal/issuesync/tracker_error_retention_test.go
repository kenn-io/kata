package issuesync

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"testing"
	"time"
)

type trackerRecordFailureStore struct {
	db.Storage
	retained bool
}

func (s *trackerRecordFailureStore) RecordIssueSyncError(_ context.Context, p db.IssueSyncErrorParams) (db.IssueSyncStatus, error) {
	s.retained = p.RetainClaim
	return db.IssueSyncStatus{}, context.DeadlineExceeded
}

// An error while recording a failure cannot erase the original delivery hazard.
func TestTrackerRecordingFailurePreservesUnknownCause(t *testing.T) {
	store := &trackerRecordFailureStore{}
	runner := NewRunner(RunnerConfig{Store: store})
	cause := errors.Join(db.ErrTrackerOutcomeUnknown, &StatusError{Message: "uncertain delivery", Ambiguous: true})
	_, err := runner.recordError(t.Context(), db.IssueSyncBinding{}, time.Now(), cause, db.ImportBatchResult{})
	require.ErrorIs(t, err, db.ErrTrackerOutcomeUnknown)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, store.retained)
}
