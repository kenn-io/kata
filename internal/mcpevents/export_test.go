package mcpevents

import (
	"net/http"
	"time"
)

// Internals the external test package needs. The service tests live in
// mcpevents_test because their PostgreSQL helper imports the daemon, which
// imports this package.
var (
	NewIncarnation     = newIncarnation
	FixtureWebhook     = fixtureWebhook
	FixtureSecret      = fixtureSecret
	CanonicalArguments = canonicalArguments
	TerminatedBody     = terminatedBody
	InboxSourceEventID = inboxSourceEventID
)

// Sealed secret roles.
const (
	SecretRoleCurrent  = secretRoleCurrent
	SecretRolePrevious = secretRolePrevious
)

// InflightCount reports the in-flight verifications reserved across principals.
func (s *Service) InflightCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

// EventsKey returns the key the service seals and signs with.
func (s *Service) EventsKey() []byte { return s.key }

// TTLParts exposes the tri-state ttlMs field.
func TTLParts(t TTLMS) (present, null bool, ms int64) { return t.set, t.null, t.value }

type eventRoundTripFunc func(*http.Request) (*http.Response, error)

func (f eventRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func padID(id int64) string {
	const digits = "0123456789"
	out := []byte("0000000")
	for i := len(out) - 1; i >= 0 && id > 0; i-- {
		out[i] = digits[id%10]
		id /= 10
	}
	return string(out)
}

// PadID renders an event id as a seven digit ULID tail for fixtures.
var PadID = padID

// EffectiveRenewInterval reports the renew interval a service built from cfg
// would use, with the constants that bound it.
func EffectiveRenewInterval(cfg ServiceConfig) (interval, ceiling time.Duration) {
	return newDeliveryState(cfg).renewEvery, claimTTL - claimMargin
}
