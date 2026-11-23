package federation

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

// A refused credential needs a change somewhere before a retry can pass: a new
// token, a rebind, or a hub-side grant. Local changes reset the schedule on the
// next pass, so the cap only bounds how long a hub-side grant waits to be
// noticed, while one binding's steady-state traffic drops from a request every
// interval to about four an hour.
const (
	refusedRetryInitialDelay = time.Minute
	refusedRetryMaxDelay     = 15 * time.Minute
	// A throttled or unavailable hub is expected to recover by itself, so its
	// schedule starts at the runner interval and stays short.
	unavailableRetryMaxDelay = 5 * time.Minute
	// maxHonoredRetryAfter bounds a hub's Retry-After hint so a bad value
	// cannot park a binding until the daemon restarts.
	maxHonoredRetryAfter = time.Hour
)

// Reasons the automatic runner waits before it contacts a binding's hub again,
// as api.FederationSyncRetry reports them.
const (
	// syncRetryRefused means the hub refused the binding's credential (401
	// or 403).
	syncRetryRefused = "refused"
	// syncRetryUnavailable means the hub throttled the request or was
	// temporarily unavailable (429 or 503).
	syncRetryUnavailable = "unavailable"
	// syncRetryRelayCrossProjectLinks means a relay hub refused the project
	// because it has cross-project issue links.
	syncRetryRelayCrossProjectLinks = "relay_cross_project_links"
)

// SyncRetries holds the automatic runner's per-binding retry schedules. The
// runner and held-poll watchers write it; status readers may read it concurrently.
// State lives only in memory: a runner that starts, or a daemon that becomes the runner leader,
// makes a fresh attempt for every binding. Synchronous passes never consult it.
type SyncRetries struct {
	mu     sync.Mutex
	states map[int64]syncRetry
	next   uint64
	// jitter returns how much to take off a delay, in [0, limit).
	jitter func(limit time.Duration) time.Duration
}

type syncRetry struct {
	api.FederationSyncRetry
	version uint64
	// delay is the unjittered delay the next failure doubles.
	delay time.Duration
	// identity fingerprints the binding and credential the failure used. A
	// different fingerprint means the operator or a successful sync changed
	// something, and the schedule starts over.
	identity [32]byte
	target   [32]byte
	// held marks a schedule a held poll recorded. Its identity leaves out the
	// sync-progress timestamps, so a push that lands while the refusal is being
	// recorded cannot erase it. blockedFor compares it that way.
	held      bool
	updatedAt time.Time
}

// NewSyncRetries returns an empty schedule set with random jitter.
func NewSyncRetries() *SyncRetries {
	return newSyncRetries(func(limit time.Duration) time.Duration {
		if limit <= 0 {
			return 0
		}
		return rand.N(limit) //nolint:gosec // G404: retry jitter spreads load; it is not a secret.
	})
}

func newSyncRetries(jitter func(time.Duration) time.Duration) *SyncRetries {
	return &SyncRetries{states: map[int64]syncRetry{}, jitter: jitter}
}

// FederationSyncRetry returns the binding's retry schedule while the runner is
// waiting on it.
func (s *SyncRetries) FederationSyncRetry(projectID int64) (api.FederationSyncRetry, bool) {
	if s == nil {
		return api.FederationSyncRetry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[projectID]
	return state.FederationSyncRetry, ok
}

// waiting returns the reason of the binding's schedule, or "" when it has
// none, so the caller knows whether blocked needs the credential's identity.
func (s *SyncRetries) waiting(projectID int64) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[projectID].Reason
}

// blocked reports whether the runner must skip the binding now. A schedule
// recorded for a different binding or credential identity is dropped first.
// The relay cross-project-links schedule ignores identity, as it always has:
// it waits for the local project to change, not the credential.
func (s *SyncRetries) blocked(projectID int64, identity [32]byte, now time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[projectID]
	if !ok {
		return false
	}
	if state.Reason != syncRetryRelayCrossProjectLinks && state.identity != identity {
		delete(s.states, projectID)
		return false
	}
	return now.Before(state.NextAttemptAt)
}

// blockedFor is blocked for a binding and credential. The mutex guards both
// the choice of identity and its comparison: a held poll's schedule compares
// heldRetryIdentity, any other compares syncRetryIdentity.
func (s *SyncRetries) blockedFor(ctx context.Context, projectID int64, binding db.FederationBinding, credential config.FederationCredential, now time.Time, version uint64) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[projectID]
	if ctx.Err() != nil || (ok && state.version != version) {
		return true
	}
	if !ok {
		return false
	}
	identity, err := syncRetryIdentity(binding, credential)
	if state.held {
		identity, err = heldRetryIdentity(binding, credential)
		if !binding.UpdatedAt.Equal(state.updatedAt) && (binding.LastSyncAt == nil || !binding.UpdatedAt.Equal(*binding.LastSyncAt)) {
			delete(s.states, projectID)
			return false
		}
	}
	if err != nil {
		return false
	}
	if state.identity != identity {
		delete(s.states, projectID)
		return false
	}
	return now.Before(state.NextAttemptAt)
}

// version returns the schedule visible when an attempt starts.
func (s *SyncRetries) version(projectID int64) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[projectID].version
}

// record schedules the binding's next attempt after a turn that completed at
// completed. A turn that ended without a retryable refusal clears the schedule.
func (s *SyncRetries) record(projectID int64, identity [32]byte, err error, interval time.Duration, completed time.Time) {
	s.recordSchedule(projectID, identity, identity, false, err, interval, completed, nil, time.Time{})
}

// recordSchedule preserves a schedule written after the turn started. held
// marks a held poll's schedule, whose identity heldRetryIdentity builds.
func (s *SyncRetries) recordSchedule(projectID int64, identity, target [32]byte, held bool, err error, interval time.Duration, completed time.Time, started *uint64, updatedAt time.Time) {
	if s == nil {
		return
	}
	reason, status, retryAfter := classifySyncRetry(err)
	s.mu.Lock()
	defer s.mu.Unlock()
	if reason == "" {
		if started == nil || s.states[projectID].version == *started {
			delete(s.states, projectID)
		}
		return
	}
	previous, ok := s.states[projectID]
	hadPrevious := ok
	if ok && started != nil && previous.version != *started && previous.target != target {
		return
	}
	if ok && (previous.Reason != reason || previous.target != target) {
		ok = false
	}
	s.next++
	next := syncRetry{identity: identity, target: target, held: held, version: s.next, updatedAt: updatedAt}
	if hadPrevious && started != nil && previous.version != *started && previous.target == target {
		next.identity, next.held = target, true
		if previous.held {
			next.updatedAt = previous.updatedAt
		}
	}
	next.Reason, next.HTTPStatus = reason, status
	next.Failures = 1
	if ok {
		next.Failures = previous.Failures + 1
	}
	wait := time.Duration(0)
	switch reason {
	case syncRetryRelayCrossProjectLinks:
		// Unchanged relay refusal schedule: no jitter, doubling to the
		// reconciler's five-minute cap, never shorter than the interval.
		next.delay = max(time.Minute, interval)
		if ok {
			next.delay = max(interval, doubledDelay(previous.delay))
		}
		wait = next.delay
	case syncRetryRefused:
		next.delay = nextRetryDelay(previous.delay, ok, max(refusedRetryInitialDelay, interval), max(refusedRetryMaxDelay, interval))
		wait = max(next.delay-s.jitter(next.delay/4), interval, min(retryAfter, maxHonoredRetryAfter))
	case syncRetryUnavailable:
		next.delay = nextRetryDelay(previous.delay, ok, interval, max(unavailableRetryMaxDelay, interval))
		wait = max(next.delay-s.jitter(next.delay/4), interval, min(retryAfter, maxHonoredRetryAfter))
	}
	next.NextAttemptAt = completed.Add(wait).UTC()
	if hadPrevious && previous.target == target && previous.NextAttemptAt.After(next.NextAttemptAt) {
		next.NextAttemptAt = previous.NextAttemptAt
	}
	s.states[projectID] = next
}

func nextRetryDelay(previous time.Duration, continuing bool, initial, limit time.Duration) time.Duration {
	if !continuing {
		return min(initial, limit)
	}
	return doubledDelayTo(previous, limit)
}

// retain drops schedules for bindings that are no longer active spokes, so a
// binding that is disabled and enabled again starts over.
func (s *SyncRetries) retain(active map[int64]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for projectID := range s.states {
		if !active[projectID] {
			delete(s.states, projectID)
		}
	}
}

func (s *SyncRetries) reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.states)
}

// classifySyncRetry finds the hub response that decides a failed turn's retry
// schedule. A refused credential outranks a throttled request because only an
// operator or hub change can clear it. The longest Retry-After hint from any
// throttled response in the turn still applies, even under a refusal. A
// refusal of one pushed issue's move (federation_project_not_enrolled) is not
// a credential refusal: the pull still ran with the same credential.
func classifySyncRetry(err error) (reason string, status int, retryAfter time.Duration) {
	if errors.Is(err, errRelayCrossProjectLinks) {
		return syncRetryRelayCrossProjectLinks, http.StatusConflict, 0
	}
	for _, hubErr := range hubStatusErrors(err) {
		switch hubErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			if federationHubErrorCode(hubErr.Body) == db.FederationProjectNotEnrolledCode {
				continue
			}
			if reason != syncRetryRefused {
				reason, status = syncRetryRefused, hubErr.StatusCode
			}
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			if reason == "" {
				reason, status = syncRetryUnavailable, hubErr.StatusCode
			}
			retryAfter = max(retryAfter, hubErr.RetryAfter)
		}
	}
	return reason, status, retryAfter
}

// hubAskedToWait reports whether the hub throttled a request or said it was
// temporarily unavailable (429 or 503).
func hubAskedToWait(err error) bool {
	for _, hubErr := range hubStatusErrors(err) {
		if hubErr.StatusCode == http.StatusTooManyRequests || hubErr.StatusCode == http.StatusServiceUnavailable {
			return true
		}
	}
	return false
}

// hubStatusErrors walks a joined or wrapped error tree in order.
func hubStatusErrors(err error) []*HubStatusError {
	var out []*HubStatusError
	var walk func(error)
	walk = func(err error) {
		if err == nil {
			return
		}
		if hubErr, ok := err.(*HubStatusError); ok { //nolint:errorlint // The walk visits every wrapped error itself.
			out = append(out, hubErr)
		}
		switch wrapped := err.(type) { //nolint:errorlint // The walk visits every wrapped error itself.
		case interface{ Unwrap() []error }:
			for _, inner := range wrapped.Unwrap() {
				walk(inner)
			}
		case interface{ Unwrap() error }:
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return out
}

// syncRetryIdentity fingerprints the binding fields and stored credential a
// sync turn depends on. A rebind, re-enable, new token, provider refresh, or a
// successful sync from another path (which moves LastSyncAt) changes it.
// Disabling or enabling a binding rewrites it and moves UpdatedAt, so a binding
// disabled and enabled again between two passes changes it too. The token is
// hashed with the rest and never kept.
func syncRetryIdentity(binding db.FederationBinding, credential config.FederationCredential) ([32]byte, error) {
	material, err := json.Marshal(struct {
		HubURL        string
		HubProjectID  int64
		HubProjectUID string
		Actor         string
		AllowInsecure bool
		PushEnabled   bool
		CreatedAt     time.Time
		UpdatedAt     time.Time
		LastSyncAt    *time.Time
		RelayConfig   *db.RelayBindingConfig
		Credential    config.FederationCredential
	}{
		binding.HubURL, binding.HubProjectID, binding.HubProjectUID, binding.Actor,
		binding.AllowInsecure, binding.PushEnabled, binding.CreatedAt, binding.UpdatedAt, binding.LastSyncAt,
		binding.RelayConfig, credential,
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("fingerprint federation binding for retry: %w", err)
	}
	return sha256.Sum256(material), nil
}

// heldRetryIdentity omits timestamps a sync can update. blockedFor checks the
// binding's update time separately to detect operator changes.
func heldRetryIdentity(binding db.FederationBinding, credential config.FederationCredential) ([32]byte, error) {
	binding.UpdatedAt, binding.LastSyncAt = time.Time{}, nil
	return syncRetryIdentity(binding, credential)
}

// parseRetryAfter reads a Retry-After header as delay seconds or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(min(seconds, int64(maxHonoredRetryAfter/time.Second))) * time.Second
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	return max(at.Sub(now), 0)
}

func (r *Runner) effectiveInterval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 30 * time.Second
}
