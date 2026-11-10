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
	identity relayRefusalIdentity
	delay    time.Duration
	due      time.Time
}

type relayRefusalIdentity struct {
	hubURL, projectUID      string
	hubProjectID            int64
	createdAt               time.Time
	bindingUID, upstreamUID string
	resetEpoch              int64
}

func refusalIdentity(b db.FederationBinding) relayRefusalIdentity {
	identity := relayRefusalIdentity{hubURL: b.HubURL, projectUID: b.HubProjectUID, hubProjectID: b.HubProjectID, createdAt: b.CreatedAt}
	if c := b.RelayConfig; c != nil {
		identity.bindingUID, identity.upstreamUID, identity.resetEpoch = c.BindingUID, c.UpstreamInstanceUID, c.ResetEpoch
	}
	return identity
}

func (states relayRefusalRetries) reconcile(spokes []activeSpokeBinding) {
	for id, state := range states {
		found := false
		for _, spoke := range spokes {
			if spoke.binding.ProjectID == id && refusalIdentity(spoke.binding) == state.identity {
				found = true
				break
			}
		}
		if !found {
			delete(states, id)
		}
	}
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
	identity := refusalIdentity(binding)
	if previous, ok := states[binding.ProjectID]; ok {
		delay = max(interval, doubledDelay(previous.delay))
	}
	states[binding.ProjectID] = relayRefusalRetry{identity: identity, delay: delay, due: completed.Add(delay)}
}

func (r *Runner) effectiveInterval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 30 * time.Second
}
