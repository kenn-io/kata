package main

import (
	"time"

	backoff "github.com/cenkalti/backoff/v7"
	"go.kenn.io/kata/internal/httpurl"
)

// BusyRetryBudget leaves room for a five-second hint and a retry.
const BusyRetryBudget = 11 * time.Second

// BusyRetry schedules up to three retries after a capacity refusal.
type BusyRetry struct {
	backoff  *backoff.ExponentialBackOff
	deadline time.Time
	budget   time.Duration
	attempts int
}

// NewBusyRetry starts a retry schedule with the given time budget.
func NewBusyRetry(budget time.Duration) *BusyRetry {
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval, bo.MaxInterval, bo.Multiplier, bo.RandomizationFactor = 250*time.Millisecond, time.Second, 2, 0.5
	bo.Reset()
	return &BusyRetry{backoff: bo, budget: budget, deadline: time.Now().Add(budget)}
}

// Next returns the suggested delay and whether another retry fits.
func (r *BusyRetry) Next(retryAfter string) (time.Duration, bool) {
	now := time.Now()
	r.attempts++
	delay := r.backoff.NextBackOff()
	if hint, valid := httpurl.RetryAfterWait(retryAfter, now); valid && hint >= 0 {
		delay = hint
	}
	return delay, r.attempts <= 3 && (r.budget <= 0 || time.Until(r.deadline) > delay)
}
