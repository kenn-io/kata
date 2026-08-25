package rootbridge

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRunnerDeliversRetainedEventsExactlyOnceWhenReconcileReturnsError(t *testing.T) {
	want := db.Event{ID: 101, Type: "issue.updated", Actor: "connector:notes"}
	delivered := make(chan db.Event, 2)
	runner := &Runner{
		Interval: time.Hour,
		reconcileFn: func(context.Context, int64) (RunResult, error) {
			return RunResult{Events: []db.Event{want}}, errors.New("terminal connector failure")
		},
		EventSink: func(event db.Event) { delivered <- event },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(1)

	select {
	case got := <-delivered:
		assert.Equal(t, want, got)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for retained runner event")
	}
	select {
	case duplicate := <-delivered:
		t.Fatalf("runner delivered event twice: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerReportsBindingReconcileError(t *testing.T) {
	reported := make(chan error, 1)
	runner := &Runner{
		Interval: time.Hour,
		reconcileFn: func(context.Context, int64) (RunResult, error) {
			return RunResult{}, errors.New("terminal connector failure")
		},
		ErrorSink: func(err error) { reported <- err },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(41)

	select {
	case err := <-reported:
		assert.EqualError(t, err, "reconcile external root binding 41: terminal connector failure")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconciliation error")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerDoesNotReportBindingCancellation(t *testing.T) {
	reported := make(chan error, 1)
	started := make(chan struct{})
	runner := &Runner{
		Interval: time.Hour,
		reconcileFn: func(ctx context.Context, _ int64) (RunResult, error) {
			close(started)
			<-ctx.Done()
			return RunResult{}, ctx.Err()
		},
		ErrorSink: func(err error) { reported <- err },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(42)
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case err := <-reported:
		t.Fatalf("runner reported cancellation as a reconciliation failure: %v", err)
	default:
	}
}

func TestRunnerDeliversCommittedProjectionBeforeLaterConnectorCallReturns(t *testing.T) {
	h := newPublishingReconcileHarness(t)
	h.client.read.Title = "Committed external title"
	h.client.read.Revision = "root-revision-with-event"
	h.client.read.UpdatedAt = h.now.Add(time.Minute)
	h.client.read.ObservedAt = h.client.read.UpdatedAt
	local := h.createLocalComment(t, "Block after the inbound transaction")
	h.client.publishResult = publishedComment(h, "published-after-event", local.Body)
	entered := make(chan struct{})
	release := make(chan struct{})
	h.client.beforePublish = func() {
		close(entered)
		<-release
	}
	delivered := make(chan db.Event, 8)
	runner := &Runner{
		Store: h.store, Reconciler: h.reconciler, Interval: time.Hour,
		EventSink: func(event db.Event) { delivered <- event },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(h.binding.ID)

	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		require.FailNow(t, "later connector call did not start")
	}
	var eventBeforeRelease db.Event
	select {
	case eventBeforeRelease = <-delivered:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NotZero(t, eventBeforeRelease.ID, "committed projection event was buffered behind connector work")
	assert.Equal(t, "issue.updated", eventBeforeRelease.Type)
}

type transientDueScanStore struct {
	db.Storage
	calls atomic.Int64
}

func (s *transientDueScanStore) ListDueExternalRootBindings(
	ctx context.Context,
	now, staleBefore time.Time,
	limit int,
) ([]db.ExternalRootBinding, error) {
	if s.calls.Add(1) == 1 {
		return nil, errors.New("transient due scan failure")
	}
	return s.Storage.ListDueExternalRootBindings(ctx, now, staleBefore, limit)
}

func TestRunnerRetriesTransientDueScanErrorOnNextTick(t *testing.T) {
	h := newReconcileHarness(t)
	store := &transientDueScanStore{Storage: h.store}
	var reconciles atomic.Int64
	var reported atomic.Int64
	runner := &Runner{
		Store: store, Interval: 5 * time.Millisecond,
		reconcileFn: func(context.Context, int64) (RunResult, error) {
			reconciles.Add(1)
			return RunResult{}, nil
		},
		ErrorSink: func(error) { reported.Add(1) },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	require.Eventually(t, func() bool {
		return store.calls.Load() >= 2 && reconciles.Load() >= 1
	}, time.Second, time.Millisecond)
	assert.Equal(t, int64(1), reported.Load())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerDoesNotReportDueScanErrorAfterCancellation(t *testing.T) {
	h := newReconcileHarness(t)
	store := &transientDueScanStore{Storage: h.store}
	store.calls.Store(-1)
	var reported atomic.Int64
	runner := &Runner{Store: store, Reconciler: h.reconciler, ErrorSink: func(error) { reported.Add(1) }}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := runner.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, reported.Load())
}

func TestRunnerDuplicateWakeAndPollConvergeOnce(t *testing.T) {
	h := newReconcileHarness(t)
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	runner := &Runner{
		Store: h.store, Reconciler: h.reconciler, Interval: 5 * time.Millisecond,
		reconcileFn: func(ctx context.Context, _ int64) (RunResult, error) {
			calls.Add(1)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return RunResult{}, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	runner.Wake(h.binding.ID)
	runner.Wake(h.binding.ID)
	<-started
	runner.Wake(h.binding.ID)
	close(release)
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestExternalRootRetryDelayCapsAndUsesDeterministicJitter(t *testing.T) {
	jitter := func(bindingID int64, _ int, base time.Duration) time.Duration {
		assert.Equal(t, int64(42), bindingID)
		return base / 10
	}
	assert.Equal(t, 33*time.Second, ExternalRootRetryDelay(42, 0, jitter))
	assert.Equal(t, 30*time.Minute, ExternalRootRetryDelay(42, 20, jitter))
}

func TestRunnerStartupPreservesConfiguredRetryStrategyForManualReconcile(t *testing.T) {
	h := newReconcileHarness(t)
	wantRetryAt := h.now.Add(47 * time.Minute)
	h.reconciler = NewReconciler(h.store, h.registry, ReconcilerConfig{
		Now:     func() time.Time { return h.now },
		RetryAt: func(db.ExternalRootBinding, time.Time) time.Time { return wantRetryAt },
	})
	started := make(chan struct{})
	runner := &Runner{
		Reconciler: h.reconciler,
		Interval:   time.Hour,
		reconcileFn: func(context.Context, int64) (RunResult, error) {
			close(started)
			return RunResult{}, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(99)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	h.client.readErr = errors.New("synthetic connector failure")
	_, err := h.reconciler.Run(t.Context(), h.binding.ID, RunOptions{Manual: true})
	require.Error(t, err)
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	require.NotNil(t, binding.NextAttemptAt)
	assert.Equal(t, wantRetryAt, *binding.NextAttemptAt)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerRecoversBindingPanicAndContinues(t *testing.T) {
	var calls atomic.Int64
	reported := make(chan error, 1)
	runner := &Runner{Interval: time.Hour, ErrorSink: func(err error) { reported <- err }, reconcileFn: func(context.Context, int64) (RunResult, error) {
		if calls.Add(1) == 1 {
			panic("private connector diagnostic")
		}
		return RunResult{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	runner.Wake(1)
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, 10*time.Millisecond)
	select {
	case err := <-reported:
		assert.EqualError(t, err, "reconcile external root binding 1: external root reconciliation panicked")
		assert.NotContains(t, err.Error(), "private connector diagnostic")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recovered panic report")
	}
	runner.Wake(2)
	require.Eventually(t, func() bool { return calls.Load() == 2 }, time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerConnectorPanicPersistsRetryBeforeReleasingClaim(t *testing.T) {
	h := newReconcileHarness(t)
	h.client.beforeReadReturn = func() { panic("private connector diagnostic") }
	h.reconciler = NewReconciler(h.store, h.registry, ReconcilerConfig{
		Now: func() time.Time { return h.now },
		RetryAt: ExternalRootRetryAt(func(_ int64, _ int, base time.Duration) time.Duration {
			return base / 10
		}),
	})
	runner := &Runner{
		Store: h.store, Reconciler: h.reconciler, Interval: time.Hour,
		Now: func() time.Time { return h.now },
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	require.Eventually(t, func() bool {
		binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
		return err == nil && binding.ConsecutiveFailures == 1
	}, time.Second, 10*time.Millisecond)
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.Empty(t, binding.ClaimToken)
	assert.Equal(t, "external connector panicked", binding.LastError)
	require.NotNil(t, binding.NextAttemptAt)
	assert.Equal(t, h.now.Add(33*time.Second), *binding.NextAttemptAt)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerLimitsConcurrencyToFour(t *testing.T) {
	h := newReconcileHarness(t)
	for i := 2; i <= 6; i++ {
		createRunnerBinding(t, h, i)
	}
	var active atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	runner := &Runner{Store: h.store, Interval: time.Hour, MaxConcurrent: 5, reconcileFn: func(ctx context.Context, _ int64) (RunResult, error) {
		current := active.Add(1)
		for {
			prior := maximum.Load()
			if current <= prior || maximum.CompareAndSwap(prior, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		return RunResult{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	for range 4 {
		<-started
	}
	assert.Equal(t, int64(4), maximum.Load())
	select {
	case <-started:
		t.Fatal("runner exceeded four concurrent reconciliations")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.Eventually(t, func() bool { return active.Load() == 0 }, time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerWakeQueueSaturationNeverBlocksProducer(t *testing.T) {
	runner := &Runner{WakeCh: make(chan int64, 1)}
	started := time.Now()
	for i := int64(1); i <= 1000; i++ {
		runner.Wake(i)
	}
	assert.Less(t, time.Since(started), 100*time.Millisecond)
	assert.Len(t, runner.WakeCh, 1)
	// The retained item remains coalesced; repeated duplicate wakes do not grow
	// the queue or block either.
	for range 1000 {
		runner.Wake(1)
	}
	assert.Len(t, runner.WakeCh, 1)
}

func TestRunnerPollFallbackExcludesPausedBinding(t *testing.T) {
	h := newReconcileHarness(t)
	_, _, err := h.store.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{
		BindingID: h.binding.ID, Actor: "operator", Reason: "operator_pause",
	})
	require.NoError(t, err)
	var calls atomic.Int64
	runner := &Runner{Store: h.store, Interval: 5 * time.Millisecond, reconcileFn: func(context.Context, int64) (RunResult, error) {
		calls.Add(1)
		return RunResult{}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	assert.Zero(t, calls.Load())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	_, _, err = h.store.ResumeExternalRootBinding(t.Context(), db.ExternalRootActionParams{BindingID: h.binding.ID, Actor: "operator"})
	require.NoError(t, err)
	ctx, cancel = context.WithCancel(t.Context())
	done = make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	require.Eventually(t, func() bool { return calls.Load() > 0 }, time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestRunnerPollRecoversStaleClaim(t *testing.T) {
	h := newReconcileHarness(t)
	old := h.now.Add(-10 * time.Minute)
	_, ok, err := h.store.ClaimExternalRootBinding(t.Context(), h.binding.ID, "stale-claim", old, old.Add(-5*time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	runner := &Runner{Store: h.store, Reconciler: h.reconciler, Interval: time.Hour, Now: func() time.Time { return h.now }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	require.Eventually(t, func() bool {
		binding, readErr := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
		return readErr == nil && binding.LastSuccessAt != nil
	}, time.Second, 10*time.Millisecond)
	binding, err := h.store.ExternalRootBindingByID(t.Context(), h.binding.ID)
	require.NoError(t, err)
	assert.Empty(t, binding.ClaimToken)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func createRunnerBinding(t *testing.T, h *reconcileHarness, index int) db.ExternalRootBinding {
	t.Helper()
	issue, _, err := h.store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: h.project.ID, Title: fmt.Sprintf("Root %d", index), Author: "tester",
	})
	require.NoError(t, err)
	binding, _, err := h.store.CreateExternalRootBinding(t.Context(), db.CreateExternalRootBindingParams{
		ProjectID: h.project.ID, IssueID: issue.ID, ConnectorInstance: "notes",
		ExternalRootKey: fmt.Sprintf("root-%d", index), ExternalAccountKey: "account-1",
		Actor: "tester", ReceiveCommentsAfter: h.boundAt,
	})
	require.NoError(t, err)
	return binding
}

func TestManualReconcileBypassesDueButNotEnabledOrClaim(t *testing.T) {
	h := newReconcileHarness(t)
	next := h.now.Add(time.Hour)
	claimedForFailure, ok, err := h.store.ClaimExternalRootBinding(
		t.Context(), h.binding.ID, "failure-claim", h.now, h.now.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = h.store.RecordExternalRootError(t.Context(), db.ExternalRootErrorParams{
		BindingID: claimedForFailure.ID, ClaimToken: claimedForFailure.ClaimToken,
		At: h.now, NextAttemptAt: next, Error: "temporary failure",
	})
	require.NoError(t, err)
	claimed, ok, err := h.store.ClaimExternalRootBindingForManualReconcile(
		t.Context(), h.binding.ID, "manual-claim", h.now, h.now.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "manual-claim", claimed.ClaimToken)
	assert.Equal(t, &next, claimed.NextAttemptAt)

	_, ok, err = h.store.ClaimExternalRootBindingForManualReconcile(
		t.Context(), h.binding.ID, "other-claim", h.now, h.now.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	assert.False(t, ok)
	_, err = h.store.ReleaseExternalRootClaim(t.Context(), h.binding.ID, "manual-claim")
	require.NoError(t, err)
	_, _, err = h.store.PauseExternalRootBinding(t.Context(), db.ExternalRootActionParams{
		BindingID: h.binding.ID, Actor: "operator", Reason: "operator_pause",
	})
	require.NoError(t, err)
	_, ok, err = h.store.ClaimExternalRootBindingForManualReconcile(
		t.Context(), h.binding.ID, "paused-claim", h.now, h.now.Add(-5*time.Minute),
	)
	require.NoError(t, err)
	assert.False(t, ok)
}
