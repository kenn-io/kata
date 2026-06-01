package db

import (
	"context"
	"time"

	backoff "github.com/cenkalti/backoff/v5"
)

const (
	defaultInitialRetryBackoff = time.Millisecond
	defaultMaxRetryBackoff     = time.Second
	defaultMaxRetryElapsed     = 30 * time.Second
)

type retryConfig struct {
	maxElapsed time.Duration
	newBackOff func() backoff.BackOff
}

// RetryTransient retries op while isTransient reports its error as a transient
// condition that may clear on a whole-operation retry. op must be safe to re-run
// in full; callers must not wrap a single statement inside an already-open
// transaction. The backend supplies isTransient (SQLite busy/locked today;
// Postgres serialization/deadlock later), so this loop stays backend-neutral.
func RetryTransient(ctx context.Context, isTransient func(error) bool, op func() error) error {
	return retryTransient(ctx, retryConfig{
		maxElapsed: defaultMaxRetryElapsed,
		newBackOff: newRetryBackOff,
	}, isTransient, op)
}

func retryTransient(ctx context.Context, cfg retryConfig, isTransient func(error) bool, op func() error) error {
	cfg = normalizedRetryConfig(cfg)
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := op()
		if err == nil {
			return struct{}{}, nil
		}
		if !isTransient(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(cfg.newBackOff()), backoff.WithMaxElapsedTime(cfg.maxElapsed))
	return err
}

func normalizedRetryConfig(cfg retryConfig) retryConfig {
	if cfg.maxElapsed <= 0 {
		cfg.maxElapsed = defaultMaxRetryElapsed
	}
	if cfg.newBackOff == nil {
		cfg.newBackOff = newRetryBackOff
	}
	return cfg
}

type retryBackOff struct {
	inner backoff.BackOff
	max   time.Duration
}

func newRetryBackOff() backoff.BackOff {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = defaultInitialRetryBackoff
	bo.MaxInterval = defaultMaxRetryBackoff
	bo.Multiplier = 2
	bo.RandomizationFactor = 0.5
	bo.Reset()
	return &retryBackOff{inner: bo, max: defaultMaxRetryBackoff}
}

func (b *retryBackOff) NextBackOff() time.Duration {
	d := b.inner.NextBackOff()
	if d > b.max {
		return b.max
	}
	return d
}

func (b *retryBackOff) Reset() { b.inner.Reset() }
