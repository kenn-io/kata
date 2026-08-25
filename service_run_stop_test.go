package kata

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunResultAppliesShutdownPolicy pins decision D1. The first case is the
// one the old Run got wrong: a worker that failed for a real reason was
// silenced because a shutdown happened to be in flight, so the error reached
// neither the caller nor a log.
func TestRunResultAppliesShutdownPolicy(t *testing.T) {
	workerFailure := errors.New("github sync worker exploded")
	fenceRejection := errors.New("worker transaction rejected")

	for _, testCase := range []struct {
		name               string
		stop               runStop
		workerResults      []error
		callerShuttingDown bool
		check              func(t *testing.T, err error)
	}{
		{
			name:               "worker failure during shutdown reaches the caller",
			stop:               runStop{reason: stopContextDone, err: context.Canceled},
			workerResults:      []error{nil, workerFailure, nil},
			callerShuttingDown: true,
			check: func(t *testing.T, err error) {
				require.ErrorIs(t, err, workerFailure)
			},
		},
		{
			name:               "cancellation during shutdown is a clean stop",
			stop:               runStop{reason: stopContextDone, err: context.Canceled},
			workerResults:      []error{nil, nil, nil},
			callerShuttingDown: true,
			check:              func(t *testing.T, err error) { assert.NoError(t, err) },
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
			name:               "a fence canceled by the shutdown it unwinds is not a failure",
			stop:               runStop{reason: stopFenceFailure, err: context.Canceled},
			workerResults:      []error{nil, nil, nil},
			callerShuttingDown: true,
			check:              func(t *testing.T, err error) { assert.NoError(t, err) },
		},
		{
			name: "only cancellation causes from a fence are a clean shutdown",
			stop: runStop{reason: stopFenceFailure, err: errors.Join(
				context.Canceled, fmt.Errorf("stopping: %w", context.Canceled))},
			workerResults:      []error{nil, nil, nil},
			callerShuttingDown: true,
			check:              func(t *testing.T, err error) { assert.NoError(t, err) },
		},
		{
			name: "a fence cancellation joined with a rejection still surfaces during shutdown",
			stop: runStop{reason: stopFenceFailure, err: errors.Join(
				context.Canceled, fenceRejection)},
			workerResults:      []error{nil, nil, nil},
			callerShuttingDown: true,
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
				testCase.stop, testCase.workerResults, testCase.callerShuttingDown))
		})
	}
}

// TestNamedWorkerResultAttributesOnlyRealFailures pins the other half of the
// policy: cancellation is how a worker reports a clean stop, and anything else
// is named after the worker that produced it.
func TestNamedWorkerResultAttributesOnlyRealFailures(t *testing.T) {
	worker := namedWorker{name: "github-sync"}

	assert.NoError(t, worker.result(nil))
	assert.NoError(t, worker.result(context.Canceled))
	assert.NoError(t, worker.result(fmt.Errorf("stopping: %w", context.Canceled)))
	assert.NoError(t, worker.result(errors.Join(
		context.Canceled, fmt.Errorf("stopping: %w", context.Canceled))))

	failure := errors.New("boom")
	err := worker.result(failure)
	require.ErrorIs(t, err, failure)
	assert.ErrorContains(t, err, "github-sync worker")

	joinedErr := worker.result(errors.Join(context.Canceled, failure))
	require.ErrorIs(t, joinedErr, failure)
	assert.ErrorContains(t, joinedErr, "github-sync worker")
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
		return runResult(stop, workerResults, false)
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
