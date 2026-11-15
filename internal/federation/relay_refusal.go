package federation

import (
	"errors"
	"time"

	"go.kenn.io/kata/internal/db"
)

// Retry state belongs to one serial automatic runner invocation. Synchronous
// passes never consult it; restarting the runner makes a fresh attempt.
type relayRefusalRetries map[int64]relayRefusalRetry

type relayRefusalRetry struct {
	delay time.Duration
	due   time.Time
}

func (states relayRefusalRetries) blocked(binding db.FederationBinding, now time.Time) bool {
	state, ok := states[binding.ProjectID]
	return ok && now.Before(state.due)
}

func (states relayRefusalRetries) record(binding db.FederationBinding, err error, interval time.Duration, completed time.Time) {
	if states == nil {
		return
	}
	if !errors.Is(err, errRelayCrossProjectLinks) {
		delete(states, binding.ProjectID)
		return
	}
	delay := max(time.Minute, interval)
	if previous, ok := states[binding.ProjectID]; ok {
		delay = max(interval, doubledDelay(previous.delay))
	}
	states[binding.ProjectID] = relayRefusalRetry{delay: delay, due: completed.Add(delay)}
}

func (r *Runner) effectiveInterval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 30 * time.Second
}
