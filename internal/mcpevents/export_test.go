package mcpevents

import (
	"context"
	"net/http"
	"net/netip"
	"time"
)

// Internals the external test package needs. The service tests live in
// mcpevents_test because their PostgreSQL helper imports the daemon, which
// imports this package.
var (
	FixtureWebhook     = fixtureWebhook
	FixtureSecret      = fixtureSecret
	CanonicalArguments = canonicalArguments
	TerminatedBody     = terminatedBody
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

// ChallengeStateCount reports how many consent and failure-budget entries the
// challenge coordinator retains.
func (s *Service) ChallengeStateCount() int {
	c := s.challenges
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recent) + len(c.hostFails) + len(c.principals)
}

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

// Rematch runs one rematch pass, as another daemon's worker would.
func (s *Service) Rematch(ctx context.Context) error { return s.rematch(ctx) }

// SetResolverForTest installs a resolver before a fixture starts serving calls.
func (w *WebhookClient) SetResolverForTest(resolve func(context.Context, string, string) ([]netip.Addr, error)) {
	w.resolve = resolve
}
