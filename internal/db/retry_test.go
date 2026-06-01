package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func alwaysTransient(error) bool { return true }

func TestRetryTransientRetriesUntilSuccess(t *testing.T) {
	attempts := 0
	err := retryTransient(context.Background(), retryConfig{
		maxElapsed: time.Second,
		newBackOff: func() backoff.BackOff { return &backoff.ZeroBackOff{} },
	}, alwaysTransient, func() error {
		attempts++
		if attempts < 4 {
			return fmt.Errorf("retry: %w", errors.New("busy"))
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, attempts)
}

func TestRetryTransientDoesNotRetryPermanentErrors(t *testing.T) {
	wantErr := errors.New("permanent")
	attempts := 0
	err := retryTransient(context.Background(), retryConfig{
		maxElapsed: time.Second,
		newBackOff: func() backoff.BackOff { return &backoff.ZeroBackOff{} },
	}, func(error) bool { return false }, func() error {
		attempts++
		return wantErr
	})
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, attempts)
}

func TestNewRetryBackOffUsesSmallJitteredPolicy(t *testing.T) {
	got, ok := newRetryBackOff().(*retryBackOff)
	require.True(t, ok)
	inner, ok := got.inner.(*backoff.ExponentialBackOff)
	require.True(t, ok)
	assert.Equal(t, time.Millisecond, inner.InitialInterval)
	assert.Equal(t, time.Second, inner.MaxInterval)
	assert.Equal(t, 2.0, inner.Multiplier)
	assert.Equal(t, 0.5, inner.RandomizationFactor)
	assert.Equal(t, time.Second, got.max)
}

func TestRetryBackOffCapsRandomizedDelayAtOneSecond(t *testing.T) {
	bo := &retryBackOff{
		inner: &sequenceBackOff{next: []time.Duration{2 * time.Second}},
		max:   time.Second,
	}
	assert.Equal(t, time.Second, bo.NextBackOff())
}

type sequenceBackOff struct{ next []time.Duration }

func (b *sequenceBackOff) NextBackOff() time.Duration {
	if len(b.next) == 0 {
		return backoff.Stop
	}
	d := b.next[0]
	b.next = b.next[1:]
	return d
}
func (b *sequenceBackOff) Reset() {}
