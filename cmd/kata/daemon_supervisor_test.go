package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDaemonWorkerGroupJoinsCancelledWorkers(t *testing.T) {
	group := newDaemonWorkerGroup()
	workerCtx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	group.Go(func() {
		<-workerCtx.Done()
		close(exited)
	})

	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	require.True(t, group.Wait(waitCtx))
	select {
	case <-exited:
	default:
		t.Fatal("Wait returned before worker exited")
	}
}

func TestDaemonWorkerGroupReportsUnjoinedWorker(t *testing.T) {
	group := newDaemonWorkerGroup()
	release := make(chan struct{})
	group.Go(func() { <-release })

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, group.Wait(waitCtx))
	close(release)
}

type testHookShutdown struct {
	started  chan struct{}
	release  chan struct{}
	draining atomic.Bool
}

func (s *testHookShutdown) BeginProducerDrain() { s.draining.Store(true) }

func (s *testHookShutdown) Shutdown(ctx context.Context) error {
	close(s.started)
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestDaemonShutdownCoordinatorDrainsHookProducersBeforeStoppingHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	workers := newDaemonWorkerGroup()
	workerCanceled := make(chan struct{})
	releaseWorker := make(chan struct{})
	workerExited := make(chan struct{})
	require.True(t, workers.Go(func() {
		<-ctx.Done()
		close(workerCanceled)
		<-releaseWorker
		close(workerExited)
	}))
	hooks := &testHookShutdown{started: make(chan struct{}), release: make(chan struct{})}
	var admissionStopped atomic.Bool
	var producerDrainStartedBeforeAdmissionStop atomic.Bool
	shutdown := startDaemonShutdownCoordinator(
		ctx, cancel, workers, hooks, func() {
			producerDrainStartedBeforeAdmissionStop.Store(hooks.draining.Load())
			admissionStopped.Store(true)
		}, time.Second,
	)

	shutdown.Trigger()
	require.True(t, admissionStopped.Load(), "Trigger must synchronously stop admission")
	require.True(t, producerDrainStartedBeforeAdmissionStop.Load(), "hook handoffs must remain open before idle admission stops")
	select {
	case <-workerCanceled:
	case <-time.After(time.Second):
		t.Fatal("worker did not observe root cancellation")
	}
	shutdown.HTTPHandlersDone(true)
	select {
	case <-hooks.started:
		t.Fatal("hook shutdown started before background producers drained")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseWorker)
	select {
	case <-hooks.started:
	case <-time.After(time.Second):
		t.Fatal("hook shutdown did not start after all producers drained")
	}
	close(hooks.release)
	result := shutdown.Wait()
	require.NoError(t, result.Err())
	select {
	case <-workerExited:
	default:
		t.Fatal("shutdown completed before worker exit")
	}
}

func TestDaemonShutdownCoordinatorWaitsForHTTPHandlersBeforeStoppingHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hooks := &testHookShutdown{started: make(chan struct{}), release: make(chan struct{})}
	shutdown := startDaemonShutdownCoordinator(
		ctx, cancel, newDaemonWorkerGroup(), hooks, nil, time.Second,
	)

	shutdown.Trigger()
	select {
	case <-hooks.started:
		t.Fatal("hook shutdown started before HTTP handlers drained")
	case <-time.After(20 * time.Millisecond):
	}
	shutdown.HTTPHandlersDone(true)
	select {
	case <-hooks.started:
	case <-time.After(time.Second):
		t.Fatal("hook shutdown did not start after HTTP handlers drained")
	}
	close(hooks.release)
	require.NoError(t, shutdown.Wait().Err())
}

func TestDaemonShutdownCoordinatorRejectsReadinessAfterShutdownStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown := startDaemonShutdownCoordinator(
		ctx,
		cancel,
		newDaemonWorkerGroup(),
		nil,
		nil,
		time.Second,
	)

	shutdown.Trigger()
	shutdown.HTTPHandlersDone(true)
	published := false
	err := shutdown.PublishReady(ctx, func() error {
		published = true
		return nil
	})

	require.ErrorIs(t, err, errDaemonStoppingBeforeReady)
	require.False(t, published)
	require.NoError(t, shutdown.Wait().Err())
}

func TestDaemonShutdownCoordinatorLetsInFlightReadinessWinBeforeShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	shutdown := startDaemonShutdownCoordinator(
		ctx,
		cancel,
		newDaemonWorkerGroup(),
		nil,
		nil,
		time.Second,
	)

	publishStarted := make(chan struct{})
	releasePublish := make(chan struct{})
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- shutdown.PublishReady(ctx, func() error {
			close(publishStarted)
			<-releasePublish
			return nil
		})
	}()
	select {
	case <-publishStarted:
	case <-time.After(time.Second):
		t.Fatal("readiness publication did not start")
	}

	triggerDone := make(chan struct{})
	go func() {
		shutdown.Trigger()
		close(triggerDone)
	}()
	select {
	case <-ctx.Done():
		t.Fatal("shutdown overtook readiness publication inside the lifecycle gate")
	case <-time.After(20 * time.Millisecond):
	}

	close(releasePublish)
	require.NoError(t, <-publishDone)
	select {
	case <-triggerDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not continue after readiness publication")
	}
	shutdown.HTTPHandlersDone(true)
	require.NoError(t, shutdown.Wait().Err())
}

func TestDaemonShutdownCoordinatorSkipsHookShutdownWhenWorkerRemainsUnjoined(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	workers := newDaemonWorkerGroup()
	workerRelease := make(chan struct{})
	require.True(t, workers.Go(func() { <-workerRelease }))
	hooks := &testHookShutdown{started: make(chan struct{}), release: make(chan struct{})}
	cleanupStarted := make(chan struct{})
	shutdown := startDaemonShutdownCoordinator(
		ctx,
		cancel,
		workers,
		hooks,
		nil,
		20*time.Millisecond,
		func(cleanupCtx context.Context) bool {
			close(cleanupStarted)
			<-cleanupCtx.Done()
			return false
		},
	)

	shutdown.Trigger()
	shutdown.HTTPHandlersDone(true)
	result := shutdown.Wait()
	require.False(t, result.SafeToCloseDependencies())
	require.ErrorContains(t, result.Err(), "background workers")
	require.ErrorContains(t, result.Err(), "platform cleanup")
	require.True(t, errors.Is(result.Err(), context.DeadlineExceeded))
	select {
	case <-hooks.started:
		t.Fatal("hook shutdown must not seal the queue while a producer remains unjoined")
	default:
	}
	select {
	case <-cleanupStarted:
	default:
		t.Fatal("platform cleanup did not start under the shared deadline")
	}
	close(workerRelease)
}

func TestDaemonShutdownCoordinatorSkipsHookShutdownWhenHTTPRemainsUnjoined(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hooks := &testHookShutdown{started: make(chan struct{}), release: make(chan struct{})}
	shutdown := startDaemonShutdownCoordinator(
		ctx,
		cancel,
		newDaemonWorkerGroup(),
		hooks,
		nil,
		20*time.Millisecond,
	)

	shutdown.Trigger()
	shutdown.HTTPHandlersDone(false)
	result := shutdown.Wait()
	require.False(t, result.SafeToCloseDependencies())
	require.ErrorContains(t, result.Err(), "HTTP handlers")
	select {
	case <-hooks.started:
		t.Fatal("hook shutdown must not seal the queue while an HTTP producer remains unjoined")
	default:
	}
}
