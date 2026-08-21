package daemon

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/activity"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestTimedClaimSweeperSkipsPassWhenDrainAdmissionIsClosed(t *testing.T) {
	controller := NewIdleController(time.Minute, nil)
	controller.Start()
	controller.Stop()
	sweeper := NewTimedClaimSweeper(nil, nil, nil)
	sweeper.IdleAdmission = controller.WaitableDrainAdmission()

	require.NoError(t, sweeper.RunOnce(context.Background(), time.Now()))
}

func TestTimedClaimSweeperRetriesImmediatelyWhenDrainAdmissionReopens(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	reopened := make(chan struct{})
	completed := make(chan struct{})
	var attempts atomic.Int32
	sweeper := NewTimedClaimSweeper(store, nil, nil)
	sweeper.Interval = time.Hour
	sweeper.IdleAdmission = func() (*activity.Lease, bool, <-chan struct{}) {
		if attempts.Add(1) == 1 {
			return nil, false, reopened
		}
		return activity.NewLease(func() { close(completed) }, nil), true, nil
	}

	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx) }()
	require.Eventually(t, func() bool { return attempts.Load() == 1 }, time.Second, time.Millisecond)

	close(reopened)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("timed-claim sweep did not complete after drain admission reopened")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestTimedClaimSweeperWaitsForCancellationAfterTerminalDrainDenial(t *testing.T) {
	var attempts atomic.Int32
	sweeper := NewTimedClaimSweeper(nil, nil, nil)
	sweeper.Interval = 5 * time.Millisecond
	sweeper.IdleAdmission = func() (*activity.Lease, bool, <-chan struct{}) {
		attempts.Add(1)
		return nil, false, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sweeper.Run(ctx) }()
	require.Eventually(t, func() bool { return attempts.Load() >= 1 }, time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, int32(1), attempts.Load(), "terminal denial must not be polled")

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
