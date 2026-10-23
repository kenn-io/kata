package mcpevents

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Failed-challenge budget (design spec §7.4): ten per hour per destination
// host and ten per hour per principal.
const (
	challengeFailureBurst  = 10
	challengeFailureWindow = time.Hour
	// recentConsentTTL covers the gap between a successful challenge and the
	// activation commit that makes consent durable, so a subscribe arriving in
	// that gap does not send a second challenge.
	recentConsentTTL   = time.Minute
	maxIdleBucketCount = 4096
)

// bucket is a token bucket where each failed challenge spends one token.
type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) refill(now time.Time) {
	if b.at.IsZero() {
		b.tokens, b.at = challengeFailureBurst, now
		return
	}
	if elapsed := now.Sub(b.at); elapsed > 0 {
		b.tokens = min(challengeFailureBurst, b.tokens+elapsed.Seconds()*challengeFailureBurst/challengeFailureWindow.Seconds())
		b.at = now
	}
}

type challengeFlight struct {
	done chan struct{}
	err  error
}

// challengeCoordinator coalesces concurrent first challenges for one
// (principal, URL) consent key and rate-limits failed ones.
type challengeCoordinator struct {
	mu         sync.Mutex
	now        func() time.Time
	flights    map[string]*challengeFlight
	recent     map[string]time.Time
	hostFails  map[string]*bucket
	principals map[string]*bucket
}

func newChallengeCoordinator(now func() time.Time) *challengeCoordinator {
	return &challengeCoordinator{
		now: now, flights: map[string]*challengeFlight{}, recent: map[string]time.Time{},
		hostFails: map[string]*bucket{}, principals: map[string]*bucket{},
	}
}

func consentKey(principalKey, callbackURL string) string { return principalKey + "\x00" + callbackURL }

// destinationHost is the lowercase host name used for rate limiting.
func destinationHost(callbackURL string) string {
	u, err := url.Parse(callbackURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// verify runs challenge at most once for the consent key among concurrent
// callers. The leader checks the failure budgets before any network
// activity; followers wait for its result and share it. A successful result
// is remembered briefly so a caller that arrives before the leader's
// activation commits does not challenge again.
func (c *challengeCoordinator) verify(ctx context.Context, principalKey, callbackURL string, challenge func(context.Context) error) error {
	key := consentKey(principalKey, callbackURL)
	host := destinationHost(callbackURL)
	c.mu.Lock()
	now := c.now()
	if at, ok := c.recent[key]; ok {
		if now.Sub(at) < recentConsentTTL {
			c.mu.Unlock()
			return nil
		}
		delete(c.recent, key)
	}
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		select {
		case <-flight.done:
			return flight.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Spend the tokens before the network is touched, so concurrent challenges
	// to distinct URLs cannot all pass the check before any failure is
	// recorded. A success or a cancelled caller gets them back.
	if !c.spendIfAllowedLocked(host, principalKey, now) {
		c.mu.Unlock()
		return resourceExhausted("callback_verification_failures", challengeFailureBurst)
	}
	flight := &challengeFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()

	err := challenge(ctx)

	c.mu.Lock()
	now = c.now()
	delete(c.flights, key)
	if err == nil {
		c.recent[key] = now
		c.refundLocked(host, principalKey)
		c.pruneLocked(now)
	} else if ctx.Err() != nil {
		// A caller cancellation is not the receiver's failure.
		c.refundLocked(host, principalKey)
	}
	flight.err = err
	c.mu.Unlock()
	close(flight.done)
	return err
}

// spendIfAllowedLocked takes one token from the host and principal buckets,
// or takes none when either is empty.
func (c *challengeCoordinator) spendIfAllowedLocked(host, principalKey string, now time.Time) bool {
	buckets := []*bucket{c.bucketLocked(c.hostFails, host), c.bucketLocked(c.principals, principalKey)}
	for _, b := range buckets {
		b.refill(now)
		if b.tokens < 1 {
			return false
		}
	}
	for _, b := range buckets {
		b.tokens--
	}
	return true
}

func (c *challengeCoordinator) refundLocked(host, principalKey string) {
	for _, b := range []*bucket{c.bucketLocked(c.hostFails, host), c.bucketLocked(c.principals, principalKey)} {
		b.tokens = min(challengeFailureBurst, b.tokens+1)
	}
}

func (c *challengeCoordinator) bucketLocked(m map[string]*bucket, key string) *bucket {
	b := m[key]
	if b == nil {
		b = &bucket{}
		m[key] = b
	}
	return b
}

// pruneLocked drops idle state so the maps stay bounded.
func (c *challengeCoordinator) pruneLocked(now time.Time) {
	for key, at := range c.recent {
		if now.Sub(at) >= recentConsentTTL {
			delete(c.recent, key)
		}
	}
	for _, m := range []map[string]*bucket{c.hostFails, c.principals} {
		if len(m) < maxIdleBucketCount {
			continue
		}
		for key, b := range m {
			b.refill(now)
			if b.tokens >= challengeFailureBurst {
				delete(m, key)
			}
		}
	}
}

// endpointCategory maps a webhook client failure to a §7.6 category. It
// never exposes the underlying error text.
func endpointCategory(err error) string {
	we := safeWebhookError(err)
	switch we.Reason {
	case "timeout":
		return "timeout"
	case "tls_error":
		return "tls_error"
	case "connection_failed", "dns_error":
		return "connection_refused"
	default:
		return "challenge_failed"
	}
}
