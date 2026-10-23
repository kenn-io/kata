package mcpevents_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func TestSubscriptionQuotas(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		t.Run("the 129th subscription of a principal is refused", func(t *testing.T) {
			h := newHarness(t, open(t))
			for i := range 128 {
				h.mustSubscribe(h.request(fmt.Sprintf("example-user/%d", i)))
			}
			assert.EqualValues(t, 1, h.recv.challenges.Load(), "consent covers every later subscription to the URL")
			_, err := h.subscribe(principalA, h.request("example-user/overflow"))
			wire := requireWire(t, err, mcpevents.CodeResourceExhausted, "subscriptions")
			assert.Equal(t, map[string]any{"limit": "subscriptions", "max": 128}, wire.Data)
			// A refresh of an existing subscription is not new capacity.
			h.mustSubscribe(h.request("example-user/0"))
		})
		t.Run("the daemon cap is shared by principals", func(t *testing.T) {
			h := newHarness(t, open(t), mcpevents.Limits{DaemonActive: 2})
			for i := range 2 {
				_, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: strconv.Itoa(100 + i)}, h.request("example-user"))
				require.NoError(t, err)
			}
			_, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "102"}, h.request("example-user"))
			wire := requireWire(t, err, mcpevents.CodeResourceExhausted, "daemon_subscriptions")
			assert.Equal(t, 2, wire.Data["max"])
		})
		t.Run("in-flight verifications count against the principal cap", func(t *testing.T) {
			h := newHarness(t, open(t), mcpevents.Limits{PrincipalActive: 2})
			h.mustSubscribe(h.request("example-user/active"))
			h.recv.gate = make(chan struct{})
			h.recv.entered = make(chan struct{}, 4)

			slow := h.request("example-user/slow")
			slow.Delivery.URL = "https://example.com/events?route=slow"
			done := make(chan error, 1)
			go func() {
				_, err := h.subscribe(principalA, slow)
				done <- err
			}()
			receive(t, h.recv.entered)

			challengesBefore := h.recv.challenges.Load()
			other := h.request("example-user/other")
			other.Delivery.URL = "https://example.com/events?route=other"
			_, err := h.subscribe(principalA, other)
			_ = requireWire(t, err, mcpevents.CodeResourceExhausted, "subscriptions")
			assert.Equal(t, challengesBefore, h.recv.challenges.Load(), "refused before any network activity")

			close(h.recv.gate)
			require.NoError(t, receive(t, done))
			// The reservation was released: nothing is in flight now.
			assert.Zero(t, h.svc.InflightCount())
		})
	})
}

func TestEndpointConsent(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		t.Run("a new secret sends no challenge", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.mustSubscribe(h.request("example-user"))
			rotated := h.request("example-user")
			rotated.Delivery.Secret = testSecret(5)
			h.mustSubscribe(rotated)
			elsewhere := h.request("example-user/other")
			elsewhere.Delivery.Secret = testSecret(6)
			h.mustSubscribe(elsewhere)
			assert.EqualValues(t, 1, h.recv.challenges.Load())
		})
		t.Run("consent is per principal", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.mustSubscribe(h.request("example-user"))
			_, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "8"}, h.request("example-user"))
			require.NoError(t, err)
			assert.EqualValues(t, 2, h.recv.challenges.Load())
		})
		t.Run("ended rows keep consent", func(t *testing.T) {
			h := newHarness(t, open(t))
			req := h.request("example-user")
			h.mustSubscribe(req)
			require.NoError(t, h.unsubscribe(req))
			h.clock.Advance(time.Hour) // expire the recent-consent cache
			h.mustSubscribe(req)
			other := h.request("example-user/other")
			h.mustSubscribe(other)
			assert.EqualValues(t, 1, h.recv.challenges.Load())
		})
		t.Run("concurrent first subscribes send exactly one challenge", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.recv.gate = make(chan struct{})
			h.recv.entered = make(chan struct{}, 8)
			const callers = 6
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			for i := range callers {
				wg.Go(func() {
					_, err := h.subscribe(principalA, h.request(fmt.Sprintf("example-user/%d", i)))
					errs <- err
				})
			}
			receive(t, h.recv.entered)
			time.Sleep(100 * time.Millisecond) //nolint:kennlint // Real database calls must reach the coalescing point while the first HTTP challenge is held.
			close(h.recv.gate)
			wg.Wait()
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}
			assert.EqualValues(t, 1, h.recv.challenges.Load())
			rows, err := h.store.ListMCPEventSubscriptions(t.Context(), principalA.Key())
			require.NoError(t, err)
			assert.Len(t, rows, callers)
			for _, row := range rows {
				assert.NotNil(t, row.VerifiedAt)
			}
		})
		t.Run("a failed challenge activates nothing and reports a category", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.recv.status.Store(500)
			_, err := h.subscribe(principalA, h.request("example-user"))
			wire := requireWire(t, err, mcpevents.CodeCallbackEndpoint, "challenge_failed")
			assert.NotContains(t, wire.Error(), "example.com")
			rows, listErr := h.store.ListMCPEventSubscriptions(t.Context(), principalA.Key())
			require.NoError(t, listErr)
			assert.Empty(t, rows)
		})
		t.Run("a failed challenge leaves existing rows untouched", func(t *testing.T) {
			h := newHarness(t, open(t))
			req := h.request("example-user")
			first := h.mustSubscribe(req)
			before := h.row(first.ID)
			h.recv.status.Store(500)
			other := h.request("example-user")
			other.Delivery.URL = "https://example.com/events?route=elsewhere"
			_, err := h.subscribe(principalA, other)
			_ = requireWire(t, err, mcpevents.CodeCallbackEndpoint, "challenge_failed")
			assert.Equal(t, before, h.row(first.ID))
		})
		t.Run("the eleventh failed challenge to one host is refused before the network", func(t *testing.T) {
			h := newHarness(t, open(t))
			h.recv.status.Store(500)
			for i := range 10 {
				req := h.request("example-user")
				req.Delivery.URL = fmt.Sprintf("https://example.com/events?route=%d", i)
				_, err := h.subscribe(principalA, req)
				_ = requireWire(t, err, mcpevents.CodeCallbackEndpoint, "challenge_failed")
			}
			require.EqualValues(t, 10, h.recv.challenges.Load())
			req := h.request("example-user")
			req.Delivery.URL = "https://example.com/events?route=11"
			_, err := h.subscribe(principalA, req)
			wire := requireWire(t, err, mcpevents.CodeResourceExhausted, "callback_verification_failures")
			assert.Equal(t, 10, wire.Data["max"])
			assert.EqualValues(t, 10, h.recv.challenges.Load(), "no request left the process")

			h.clock.Advance(time.Hour)
			_, err = h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeCallbackEndpoint, "challenge_failed")
			assert.EqualValues(t, 11, h.recv.challenges.Load(), "the bucket refills")
		})
		t.Run("failed challenges are also limited per principal", func(t *testing.T) {
			h := newHarness(t, open(t))
			for i := range 10 {
				req := h.request("example-user")
				// The test certificate does not name example.net, so each of these fails TLS.
				req.Delivery.URL = fmt.Sprintf("https://h%d.example.net/events", i)
				_, err := h.subscribe(principalA, req)
				require.Error(t, err, "iteration %d", i)
				_ = requireWire(t, err, mcpevents.CodeCallbackEndpoint, "tls_error")
			}
			req := h.request("example-user")
			req.Delivery.URL = "https://fresh.example.net/events"
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeResourceExhausted, "callback_verification_failures")
			// Another principal has its own budget.
			_, err = h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "8"}, req)
			_ = requireWire(t, err, mcpevents.CodeCallbackEndpoint, "tls_error")
		})
	})
}

func TestKeyStates(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		build := func(keys mcpevents.KeyProvider) *mcpevents.Service {
			svc, err := mcpevents.NewService(mcpevents.ServiceConfig{Store: store, Keys: keys, Authority: &fakeAuthority{authorized: &fakeAuthorized{}}})
			require.NoError(t, err)
			return svc
		}
		fresh := &memKeys{}
		first := build(fresh)
		assert.True(t, first.Availability().Enabled)
		assert.Equal(t, 1, fresh.creates, "the first service creates the key")
		stored, ok, err := store.MCPEventsKeyCheck(t.Context())
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, mcpevents.KeyCheck(fresh.key), stored)

		t.Run("an identical key joins", func(t *testing.T) {
			second := build(&memKeys{key: fresh.key})
			assert.True(t, second.Availability().Enabled)
		})
		t.Run("a different key disables Events and never accepts subscriptions", func(t *testing.T) {
			other := newMemKeys(0x77)
			svc := build(other)
			assert.Equal(t, mcpevents.Availability{Reason: mcpevents.ReasonKeyMismatch}, svc.Availability())
			_, err := svc.Subscribe(t.Context(), principalA, allScope, mcpevents.SubscribeRequest{Name: mcpevents.EventName})
			wire := requireWire(t, err, mcpevents.CodeUnavailable, mcpevents.ReasonKeyMismatch)
			assert.GreaterOrEqual(t, wire.Code, -32019)
			assert.LessOrEqual(t, wire.Code, -32000)
			assert.Zero(t, other.creates)
		})
		t.Run("a restored database with no key stays disabled and creates no key", func(t *testing.T) {
			restored := &memKeys{}
			svc := build(restored)
			assert.Equal(t, mcpevents.Availability{Reason: mcpevents.ReasonKeyUnavailable}, svc.Availability())
			assert.Zero(t, restored.creates, "the key is never silently regenerated")
			for _, call := range []func() error{
				func() error { _, err := svc.List(t.Context(), principalA); return err },
				func() error {
					_, err := svc.Subscribe(t.Context(), principalA, allScope, mcpevents.SubscribeRequest{Name: mcpevents.EventName})
					return err
				},
				func() error {
					return svc.Unsubscribe(t.Context(), principalA, mcpevents.UnsubscribeRequest{Name: mcpevents.EventName})
				},
				func() error { _, err := svc.Status(t.Context(), principalA); return err },
			} {
				_ = requireWire(t, call(), mcpevents.CodeUnavailable, mcpevents.ReasonKeyUnavailable)
			}
			// Ordinary reads on the same store are unaffected.
			_, err := store.ListProjects(t.Context())
			require.NoError(t, err)
		})
		t.Run("a corrupt key stays disabled and is not replaced", func(t *testing.T) {
			corrupt := &memKeys{loadErr: assert.AnError}
			svc := build(corrupt)
			assert.Equal(t, mcpevents.Availability{Reason: mcpevents.ReasonKeyUnavailable}, svc.Availability())
			assert.Zero(t, corrupt.creates)
		})
		t.Run("without a provider Events is not configured", func(t *testing.T) {
			svc := build(nil)
			assert.Equal(t, mcpevents.Availability{Reason: mcpevents.ReasonNotConfigured}, svc.Availability())
		})
	})
}

func TestRestoredCiphertextWorksWithTheRestoredKey(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)

		// A second process holding the same key reads the same rows and can open
		// the stored secret.
		again, err := mcpevents.NewService(mcpevents.ServiceConfig{Store: store, Keys: &memKeys{key: h.keys.key}, Authority: h.auth, Webhook: h.recv.client, Now: h.clock.Now})
		require.NoError(t, err)
		require.True(t, again.Availability().Enabled)
		statuses, err := again.Status(t.Context(), principalA)
		require.NoError(t, err)
		require.Len(t, statuses, 1)
		assert.Equal(t, first.Cursor, statuses[0].Cursor)
		secret, err := mcpevents.Open(again.EventsKey(), first.ID, mcpevents.SecretRoleCurrent, h.row(first.ID).SecretEnc)
		require.NoError(t, err)
		assert.Equal(t, testSecret(1), string(secret))
	})
}

func TestConcurrentChallengesCannotOverspendTheFailureBudget(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		h.recv.status.Store(500)
		h.recv.gate = make(chan struct{})
		h.recv.entered = make(chan struct{}, 32)
		const callers = 12
		errs := make(chan error, callers)
		for i := range callers {
			go func() {
				req := h.request("example-user")
				req.Delivery.URL = fmt.Sprintf("https://example.com/events?route=%d", i)
				_, err := h.subscribe(principalA, req)
				errs <- err
			}()
		}
		// Ten challenges reach the network and wait; the rest are refused
		// before any network activity, while those ten are still in flight.
		for range 10 {
			receive(t, h.recv.entered)
		}
		for range callers - 10 {
			_ = requireWire(t, receive(t, errs), mcpevents.CodeResourceExhausted, "callback_verification_failures")
		}
		close(h.recv.gate)
		for range 10 {
			_ = requireWire(t, receive(t, errs), mcpevents.CodeCallbackEndpoint, "challenge_failed")
		}
		assert.EqualValues(t, 10, h.recv.challenges.Load())
	})
}

func TestCancelledChallengeIsInterruptedNotFailed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		h.recv.gate = make(chan struct{})
		h.recv.entered = make(chan struct{}, 4)
		defer close(h.recv.gate)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := h.svc.Subscribe(ctx, principalA, allScope, h.request("example-user"))
			done <- err
		}()
		receive(t, h.recv.entered)
		cancel()
		_ = requireWire(t, receive(t, done), mcpevents.CodeUnavailable, "verification_interrupted")
	})
}

func TestDaemonCapIsCheckedBeforeTheChallenge(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store, mcpevents.Limits{DaemonActive: 1})
		h.mustSubscribe(h.request("example-user"))
		before := h.recv.challenges.Load()
		req := h.request("example-user")
		req.Delivery.URL = "https://example.com/events?route=other"
		_, err := h.subscribe(mcpevents.Principal{Authority: mcpevents.AuthorityDBToken, Subject: "9"}, req)
		_ = requireWire(t, err, mcpevents.CodeResourceExhausted, "daemon_subscriptions")
		assert.Equal(t, before, h.recv.challenges.Load(), "refused before any network activity")
	})
}

type invalidKeys struct{ create bool }

func (k invalidKeys) Load(context.Context) ([]byte, error) {
	if k.create {
		return nil, mcpevents.ErrKeyMissing
	}
	return make([]byte, 16), nil
}
func (invalidKeys) Create(context.Context) ([]byte, error) { return make([]byte, 24), nil }

func TestInvalidProviderKeyDoesNotPoisonDatabase(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprintf("create=%t", create), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				cfg := mcpevents.ServiceConfig{Store: store, Keys: invalidKeys{create}, Authority: &fakeAuthority{authorized: &fakeAuthorized{}}}
				svc, err := mcpevents.NewService(cfg)
				require.NoError(t, err)
				assert.Equal(t, mcpevents.Availability{Reason: mcpevents.ReasonKeyUnavailable}, svc.Availability())
				_, exists, err := store.MCPEventsKeyCheck(t.Context())
				require.NoError(t, err)
				assert.False(t, exists)
				cfg.Keys = newMemKeys(0x11)
				svc, err = mcpevents.NewService(cfg)
				require.NoError(t, err)
				assert.True(t, svc.Availability().Enabled, "correcting invalid key material must recover")
			})
		})
	}
}
