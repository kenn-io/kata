package kata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestServiceRunTreatsCallerDeadlineAsCleanShutdown(t *testing.T) {
	service, err := New(t.Context(), Config{
		DSN:  filepath.Join(t.TempDir(), "service.db"),
		Auth: AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })

	runCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond) //nolint:kennlint // the deadline is the expected result; Run serves until the caller deadline ends it
	defer cancel()

	require.NoError(t, service.Run(runCtx))
}

// TestRunResultAppliesShutdownPolicy pins decision D1. The first case is the
// one the old Run got wrong: a worker that failed for a real reason was
// silenced because a shutdown happened to be in flight, so the error reached
// neither the caller nor a log.
func TestRunResultAppliesShutdownPolicy(t *testing.T) {
	workerFailure := errors.New("github sync worker exploded")
	fenceRejection := errors.New("worker transaction rejected")

	for _, testCase := range []struct {
		name          string
		stop          runStop
		workerResults []error
		callerStop    error
		check         func(t *testing.T, err error)
	}{
		{
			name:          "worker failure during shutdown reaches the caller",
			stop:          runStop{reason: stopContextDone, err: context.Canceled},
			workerResults: []error{nil, workerFailure, nil},
			callerStop:    context.Canceled,
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, workerFailure)
			},
		},
		{
			name:          "cancellation during shutdown is a clean stop",
			stop:          runStop{reason: stopContextDone, err: context.Canceled},
			workerResults: []error{nil, nil, nil},
			callerStop:    context.Canceled,
			check:         func(t *testing.T, err error) { assert.NoError(t, err) },
		},
		{
			name:          "worker failure with no shutdown reaches the caller",
			stop:          runStop{reason: stopWorkerExit, err: workerFailure},
			workerResults: []error{workerFailure, nil, nil},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, workerFailure)
			},
		},
		{
			name:          "fence rejection is reported as a fence failure",
			stop:          runStop{reason: stopFenceFailure, err: fenceRejection},
			workerResults: []error{nil, nil, nil},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, fenceRejection)
				assert.ErrorContains(t, err, "worker transaction fence")
			},
		},
		{
			name:          "fence rejection outranks a worker failure",
			stop:          runStop{reason: stopFenceFailure, err: fenceRejection},
			workerResults: []error{workerFailure, nil, nil},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, fenceRejection)
			},
		},
		{
			name:          "a fence canceled by the shutdown it unwinds is not a failure",
			stop:          runStop{reason: stopFenceFailure, err: context.Canceled},
			workerResults: []error{nil, nil, nil},
			callerStop:    context.Canceled,
			check:         func(t *testing.T, err error) { assert.NoError(t, err) },
		},
		{
			name: "only cancellation causes from a fence are a clean shutdown",
			stop: runStop{reason: stopFenceFailure, err: errors.Join(
				context.Canceled, fmt.Errorf("stopping: %w", context.Canceled))},
			workerResults: []error{nil, nil, nil},
			callerStop:    context.Canceled,
			check:         func(t *testing.T, err error) { assert.NoError(t, err) },
		},
		{
			name: "a fence cancellation joined with a rejection still surfaces during shutdown",
			stop: runStop{reason: stopFenceFailure, err: errors.Join(
				context.Canceled, fenceRejection)},
			workerResults: []error{nil, nil, nil},
			callerStop:    context.Canceled,
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, fenceRejection)
				assert.ErrorContains(t, err, "worker transaction fence")
			},
		},
		{
			name: "a fence deadline joined with a rejection still surfaces during shutdown",
			stop: runStop{reason: stopFenceFailure, err: errors.Join(
				context.DeadlineExceeded, fenceRejection)},
			workerResults: []error{nil, nil, nil},
			callerStop:    context.DeadlineExceeded,
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, fenceRejection)
				assert.ErrorContains(t, err, "worker transaction fence")
			},
		},
		{
			name:          "a worker-initiated fence cancellation still surfaces",
			stop:          runStop{reason: stopFenceFailure, err: context.Canceled},
			workerResults: []error{nil, nil, nil},
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, context.Canceled)
				assert.ErrorContains(t, err, "worker transaction fence")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, runResult(
				testCase.stop, testCase.workerResults, testCase.callerStop))
		})
	}
}

// TestNamedWorkerResultAttributesOnlyRealFailures pins the other half of the
// policy: cancellation is how a worker reports a clean stop, and anything else
// is named after the worker that produced it.
func TestNamedWorkerResultAttributesOnlyRealFailures(t *testing.T) {
	worker := namedWorker{name: "github-sync"}

	assert.NoError(t, worker.result(nil, nil))
	assert.NoError(t, worker.result(context.Canceled, context.Canceled))
	assert.NoError(t, worker.result(
		fmt.Errorf("stopping: %w", context.Canceled), context.Canceled))
	assert.NoError(t, worker.result(errors.Join(
		context.Canceled, fmt.Errorf("stopping: %w", context.Canceled)), context.Canceled))
	assert.NoError(t, worker.result(context.DeadlineExceeded, context.DeadlineExceeded))
	require.ErrorIs(t, worker.result(sql.ErrTxDone, context.Canceled), sql.ErrTxDone)
	require.ErrorIs(t, worker.result(
		fmt.Errorf("commit: %w", sql.ErrTxDone), context.DeadlineExceeded), sql.ErrTxDone)
	assert.NoError(t, worker.result(errors.Join(
		sql.ErrTxDone,
		fmt.Errorf("list due notification issues: %w", context.Canceled),
		context.Canceled,
	), context.Canceled))

	require.ErrorIs(t, worker.result(sql.ErrTxDone, nil), sql.ErrTxDone)

	failure := errors.New("boom")
	err := worker.result(failure, nil)
	require.ErrorIs(t, err, failure)
	assert.ErrorContains(t, err, "github-sync worker")

	joinedErr := worker.result(errors.Join(context.DeadlineExceeded, failure), context.DeadlineExceeded)
	require.ErrorIs(t, joinedErr, failure)
	assert.ErrorContains(t, joinedErr, "github-sync worker")

	rollbackErr := worker.result(errors.Join(sql.ErrTxDone, context.Canceled, failure), context.Canceled)
	require.ErrorIs(t, rollbackErr, failure)
	assert.ErrorContains(t, rollbackErr, "github-sync worker")
}

func TestNamedWorkerResultTreatsCanceledSweeperCommitAsCleanStop(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "sweeper.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		sweeper := daemon.NewDueNotificationSweeper(
			&cancelSweeperCommitStore{Store: store, cancel: cancel}, daemon.EventPublisher{}, "UTC")

		err := sweeper.Run(ctx)
		require.ErrorIs(t, err, sql.ErrTxDone)
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, (namedWorker{name: "due-notification"}).result(err, ctx.Err()))
	})
}

type cancelSweeperCommitStore struct {
	*sqlitestore.Store
	cancel context.CancelFunc
}

func (s *cancelSweeperCommitStore) EnsureScheduledReachedFloor(ctx context.Context, at time.Time) (time.Time, error) {
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	floor, err := db.EnsureScheduledReachedFloorTx(ctx, tx, at)
	if err != nil {
		return time.Time{}, err
	}
	s.cancel()
	// Let database/sql finish its automatic rollback before the commit.
	synctest.Wait()
	return floor, tx.Commit()
}

// TestFenceRecorderPreservesStopPriorityAcrossInternalUnwind pins the boundary
// between a fence failure that stopped Run and cancellation noise caused by
// Run unwinding a stop it already recorded.
func TestFenceRecorderPreservesStopPriorityAcrossInternalUnwind(t *testing.T) {
	workerFailure := errors.New("federation worker failed")
	fenceRejection := errors.New("worker transaction rejected")
	resultAfterDrain := func(fence *fenceRecorder, stop runStop, workerResults []error) error {
		if fenceErr := fence.first(); fenceErr != nil {
			stop = runStop{reason: stopFenceFailure, err: fenceErr}
		}
		return runResult(stop, workerResults, nil)
	}

	t.Run("cancellation caused by internal unwind preserves the worker failure", func(t *testing.T) {
		fence := &fenceRecorder{cancel: func() {}}
		stop := runStop{reason: stopWorkerExit, err: workerFailure}

		fence.beginUnwind()
		require.ErrorIs(t, fence.record(context.Canceled), context.Canceled)

		err := resultAfterDrain(fence, stop, []error{workerFailure})
		require.ErrorIs(t, err, workerFailure)
		assert.NotErrorIs(t, err, context.Canceled)
	})

	t.Run("mixed fence failure during internal unwind still outranks the worker", func(t *testing.T) {
		fence := &fenceRecorder{cancel: func() {}}
		stop := runStop{reason: stopWorkerExit, err: workerFailure}

		fence.beginUnwind()
		require.Error(t, fence.record(errors.Join(context.Canceled, fenceRejection)))

		err := resultAfterDrain(fence, stop, []error{workerFailure})
		require.ErrorIs(t, err, fenceRejection)
		assert.ErrorContains(t, err, "worker transaction fence")
	})

	t.Run("fence cancellation before internal unwind remains the stop reason", func(t *testing.T) {
		fence := &fenceRecorder{cancel: func() {}}
		require.ErrorIs(t, fence.record(context.Canceled), context.Canceled)

		fence.beginUnwind()

		err := resultAfterDrain(
			fence, runStop{reason: stopContextDone, err: context.Canceled}, []error{nil})
		require.ErrorIs(t, err, context.Canceled)
		assert.ErrorContains(t, err, "worker transaction fence")
	})
}
