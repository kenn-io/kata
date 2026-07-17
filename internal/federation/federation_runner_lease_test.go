package federation

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestRunnerCancelsInFlightPassWhenLeaseIsLost(t *testing.T) {
	store := &blockingLeaseStore{
		passStarted: make(chan struct{}),
		released:    make(chan struct{}),
	}
	runnerErrors := make(chan error, 1)
	var errorOnce sync.Once
	runner := &Runner{DB: store, Interval: time.Hour, OnError: func(err error) {
		errorOnce.Do(func() { runnerErrors <- err })
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	select {
	case <-store.passStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "federation pass did not start")
	}
	store.lost.Store(true)
	select {
	case <-store.released:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "lease loss did not cancel the in-flight pass")
	}
	select {
	case err := <-runnerErrors:
		assert.ErrorIs(t, err, errTestFederationLeaseLost)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "lease loss was not reported")
	}

	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

var errTestFederationLeaseLost = errors.New("test federation lease lost")

type blockingLeaseStore struct {
	db.Storage
	lost        atomic.Bool
	startOnce   sync.Once
	releaseOnce sync.Once
	passStarted chan struct{}
	released    chan struct{}
}

func (s *blockingLeaseStore) AcquireFederationRunnerLease(ctx context.Context) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() error {
		s.releaseOnce.Do(func() { close(s.released) })
		return nil
	}, nil
}

func (s *blockingLeaseStore) ValidateFederationRunnerLease(context.Context) error {
	if s.lost.Load() {
		return errTestFederationLeaseLost
	}
	return nil
}

func (s *blockingLeaseStore) ListFederationBindings(ctx context.Context) ([]db.FederationBinding, error) {
	s.startOnce.Do(func() { close(s.passStarted) })
	<-ctx.Done()
	return nil, ctx.Err()
}
