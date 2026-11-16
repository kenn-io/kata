package mcpevents_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"maps"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
	"go.kenn.io/kata/internal/notification"
)

// hit is one delivery the receiver saw.
type hit struct {
	id, timestamp, signature, subscription, contentType string
	body                                                []byte
	typ                                                 string
}

// sink is a receiver whose answers the test scripts.
type sink struct {
	mu   sync.Mutex
	hits []hit
	// respond picks the answer for the nth request (1-based).
	respond func(n int, h hit) (int, http.Header)
	hitCh   chan hit
}

func newSink() *sink { return &sink{hitCh: make(chan hit, 64)} }

func (s *sink) handle(w http.ResponseWriter, r *http.Request, raw []byte) {
	var body struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &body)
	h := hit{
		id: r.Header.Get("Webhook-Id"), timestamp: r.Header.Get("Webhook-Timestamp"), signature: r.Header.Get("Webhook-Signature"),
		subscription: r.Header.Get("X-MCP-Subscription-Id"), contentType: r.Header.Get("Content-Type"), body: raw, typ: body.Type,
	}
	s.mu.Lock()
	s.hits = append(s.hits, h)
	n := len(s.hits)
	respond := s.respond
	s.mu.Unlock()
	select {
	case s.hitCh <- h:
	default:
	}
	status, header := http.StatusOK, http.Header(nil)
	if respond != nil {
		status, header = respond(n, h)
	}
	maps.Copy(w.Header(), header)
	w.WriteHeader(status)
}

func (s *sink) all() []hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hit(nil), s.hits...)
}

func (s *sink) count() int { return len(s.all()) }

// always answers every request with status.
func always(status int, header ...string) func(int, hit) (int, http.Header) {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	return func(int, hit) (int, http.Header) { return status, h }
}

func newDelivery(t *testing.T, store db.Storage, mutate ...func(*mcpevents.ServiceConfig)) (*harness, *sink) {
	t.Helper()
	h := newHarnessWith(t, store, func(cfg *mcpevents.ServiceConfig) {
		for _, m := range mutate {
			m(cfg)
		}
	})
	sk := newSink()
	h.recv.deliver = sk.handle
	return h, sk
}

// drain re-evaluates idle subscriptions, then delivers everything due.
func (h *harness) drain() {
	h.t.Helper()
	h.svc.Recheck()
	require.NoError(h.t, h.svc.Drain(h.t.Context()))
}

// drainDue delivers only what is already due, leaving idle rows alone.
func (h *harness) drainDue() {
	h.t.Helper()
	require.NoError(h.t, h.svc.Drain(h.t.Context()))
}

func verifySignature(t *testing.T, h hit, secrets ...string) {
	t.Helper()
	_, err := strconv.ParseInt(h.timestamp, 10, 64)
	require.NoError(t, err)
	parts := strings.Fields(h.signature)
	require.Len(t, parts, len(secrets))
	for i, secret := range secrets {
		key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
		require.NoError(t, err)
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(h.id + "." + h.timestamp + "."))
		_, _ = mac.Write(h.body)
		assert.Equal(t, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), parts[i])
	}
}

func TestDeliveryAcknowledgesAPingOn2xx(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)

		h.drain()

		hits := sk.all()
		require.Len(t, hits, 1)
		first := hits[0]
		assert.True(t, strings.HasPrefix(first.id, "evt_"), first.id)
		assert.Equal(t, result.ID, first.subscription)
		assert.Equal(t, "application/json", first.contentType)
		verifySignature(t, first, testSecret(1))
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(first.body, &envelope))
		assert.Equal(t, map[string]any{
			"eventId": first.id, "name": mcpevents.EventName, "timestamp": "2026-10-20T12:00:00Z",
			"data": map[string]any{"recipient": "example-user"}, "cursor": nil,
		}, envelope, "a ping names only the recipient and carries no replay cursor")

		row := h.row(result.ID)
		assert.False(t, row.Dirty)
		assert.Empty(t, row.PendingKind)
		assert.Zero(t, row.AttemptCount)
		assert.Nil(t, row.NextAttemptAt, "a settle leaves nothing hidden from the due list")
		assert.Empty(t, row.ClaimToken)
		require.NotNil(t, row.LastDeliveryAt)
		assert.Zero(t, row.FailedAttempts)

		h.drain()
		assert.Equal(t, 1, sk.count(), "a clean subscription sends nothing")
	})
}

// Changes marked before the worker runs become one ping; a change after that
// ping is acknowledged becomes a new one.
func TestDeliveryCoalescesChangesIntoOnePing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		h.touch(result.ID)
		h.touch(result.ID)

		h.drain()
		require.Equal(t, 1, sk.count())

		h.touch(result.ID)
		h.drain()
		hits := sk.all()
		require.Len(t, hits, 2)
		assert.NotEqual(t, hits[0].id, hits[1].id, "each ping is a new event")
	})
}

// A change marked while a ping is retrying waits behind it, then goes out as
// one more ping.
func TestDeliveryChangeDuringARetryFollowsIt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = func(n int, _ hit) (int, http.Header) {
			if n == 1 {
				return http.StatusInternalServerError, nil
			}
			return http.StatusOK, nil
		}
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		h.drain()
		require.Equal(t, 1, sk.count())

		h.touch(result.ID)
		h.touch(result.ID)
		h.drainDue()
		require.Equal(t, 1, sk.count(), "the retry is not due yet and nothing jumps ahead of it")

		h.clock.Advance(30 * time.Second)
		h.drainDue()

		hits := sk.all()
		require.Len(t, hits, 3, "the retry, then one ping for both later changes")
		assert.Equal(t, hits[0].id, hits[1].id)
		assert.NotEqual(t, hits[1].id, hits[2].id)
		row := h.row(result.ID)
		assert.False(t, row.Dirty)
		assert.Empty(t, row.PendingKind)
	})
}

func TestDeliverySignsWithBothKeysDuringARotationWindow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		req.Delivery.Secret = testSecret(2)
		h.mustSubscribe(req)
		h.touch(first.ID)
		h.drain()
		h.touch(first.ID)
		h.clock.Advance(61 * time.Second)
		h.drain()

		hits := sk.all()
		require.Len(t, hits, 2)
		verifySignature(t, hits[0], testSecret(2), testSecret(1))
		verifySignature(t, hits[1], testSecret(2))
		assert.Equal(t, first.ID, hits[1].subscription)
	})
}

// SEP-3415 makes a 410 or 413 a non-retryable failure of that one delivery
// that leaves the subscription alone.
func TestDelivery410And413DropOnePingOnly(t *testing.T) {
	for _, status := range []int{http.StatusGone, http.StatusRequestEntityTooLarge} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				h, sk := newDelivery(t, store)
				sk.respond = func(n int, _ hit) (int, http.Header) {
					if n == 1 {
						return status, nil
					}
					return http.StatusOK, nil
				}
				result := h.mustSubscribe(h.request("example-user"))
				h.touch(result.ID)

				h.drain()

				require.Equal(t, 1, sk.count(), "the dropped ping is not retried")
				row := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateActive, row.State)
				assert.Equal(t, 1, row.DeadLetterCount)
				assert.Equal(t, "http_4xx", row.LastError)
				assert.Empty(t, row.PendingKind)
				assert.Nil(t, row.NextAttemptAt)
				assert.Zero(t, row.FailedAttempts, "a dropped delivery does not age the endpoint")

				h.touch(result.ID)
				h.drain()
				require.Equal(t, 2, sk.count(), "the next change is still delivered")
				assert.NotEqual(t, sk.all()[0].id, sk.all()[1].id)
			})
		})
	}
}

func TestDeliveryRetriesOnTheFixedSchedule(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		callsBefore := h.auth.calls.Load()

		delays := []time.Duration{30 * time.Second, time.Minute, 3 * time.Minute, 8 * time.Minute}
		h.drain()
		for i, delay := range delays {
			require.Equal(t, i+1, sk.count())
			row := h.row(result.ID)
			assert.Equal(t, i+1, row.AttemptCount, "the attempt count is persisted before the send")
			require.NotNil(t, row.NextAttemptAt)
			assert.Equal(t, h.clock.Now().Add(delay), *row.NextAttemptAt)
			assert.Equal(t, "http_5xx", row.LastError)
			assert.Equal(t, i+1, row.FailedAttempts)
			// One second early nothing is sent.
			h.clock.Advance(delay - time.Second)
			h.drainDue()
			require.Equal(t, i+1, sk.count())
			h.clock.Advance(time.Second)
			h.drainDue()
		}

		require.Equal(t, 5, sk.count(), "exactly five attempts")
		ids := map[string]bool{}
		for _, h := range sk.all() {
			ids[h.id] = true
		}
		assert.Len(t, ids, 1, "every retry reuses the webhook-id")
		row := h.row(result.ID)
		assert.Equal(t, 1, row.DeadLetterCount)
		assert.Empty(t, row.PendingKind)
		assert.Nil(t, row.NextAttemptAt)
		assert.Zero(t, row.AttemptCount)
		assert.Equal(t, db.MCPEventStateActive, row.State)
		assert.GreaterOrEqual(t, h.auth.calls.Load()-callsBefore, int64(5), "every attempt reacquires the principal")
		h.clock.Advance(time.Hour)
		h.drainDue()
		assert.Equal(t, 5, sk.count())
	})
}

func TestDeliveryAddsBoundedJitter(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.Jitter = func() float64 { return 0.999 } })
		sk.respond = always(http.StatusInternalServerError)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		h.drain()
		row := h.row(result.ID)
		require.NotNil(t, row.NextAttemptAt)
		wait := row.NextAttemptAt.Sub(h.clock.Now())
		assert.Greater(t, wait, 30*time.Second)
		assert.LessOrEqual(t, wait, 33*time.Second)
	})
}

func TestDeliveryHonorsRetryAfterUpToFiveMinutes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusServiceUnavailable, "Retry-After", "3600")
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		h.drain()
		row := h.row(result.ID)
		require.NotNil(t, row.NextAttemptAt)
		assert.Equal(t, h.clock.Now().Add(5*time.Minute), *row.NextAttemptAt)
	})
}

func TestDeliveryDeadEndpointAgesOutOnlyWithFailures(t *testing.T) {
	seed := func(t *testing.T, h *harness, id string, since time.Duration, attempts int) {
		t.Helper()
		row := h.row(id)
		at := h.clock.Now().Add(-since)
		row.FailedSince, row.FailedAttempts = &at, attempts
		require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
	}
	t.Run("19 failures after 7 days keep the subscription active", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store db.Storage) {
			h, sk := newDelivery(t, store)
			sk.respond = always(http.StatusInternalServerError)
			result := h.mustSubscribe(h.request("example-user"))
			seed(t, h, result.ID, 8*24*time.Hour, 18)
			h.touch(result.ID)
			h.drain()
			row := h.row(result.ID)
			assert.Equal(t, 19, row.FailedAttempts)
			assert.Equal(t, db.MCPEventStateActive, row.State)
		})
	})
	t.Run("20 failures after 7 days end it with a terminated notice", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store db.Storage) {
			h, sk := newDelivery(t, store)
			sk.respond = always(http.StatusInternalServerError)
			result := h.mustSubscribe(h.request("example-user"))
			seed(t, h, result.ID, 8*24*time.Hour, 19)
			h.touch(result.ID)
			h.drain()

			row := h.row(result.ID)
			assert.Equal(t, db.MCPEventStateEnded, row.State)
			assert.Equal(t, "endpoint_dead", row.EndReason)
			assert.Equal(t, db.MCPEventPendingTerminated, row.PendingKind)
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Code int `json:"code"`
					Data struct {
						Reason string `json:"reason"`
					} `json:"data"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(row.PendingBody, &body))
			assert.Equal(t, "terminated", body.Type)
			assert.Equal(t, mcpevents.CodeForbidden, body.Error.Code)
			assert.Equal(t, "endpoint_dead", body.Error.Data.Reason)
			assert.Equal(t, row.PendingBody, sk.all()[sk.count()-1].body, "the notice was attempted")
		})
	})
	t.Run("a young failure run never ends it", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store db.Storage) {
			h, sk := newDelivery(t, store)
			sk.respond = always(http.StatusInternalServerError)
			result := h.mustSubscribe(h.request("example-user"))
			seed(t, h, result.ID, 6*24*time.Hour, 50)
			h.touch(result.ID)
			h.drain()
			assert.Equal(t, db.MCPEventStateActive, h.row(result.ID).State)
		})
	})
	t.Run("an idle subscription never ages out", func(t *testing.T) {
		forEachBackend(t, func(t *testing.T, store db.Storage) {
			h, _ := newDelivery(t, store)
			req := h.request("example-user")
			req.TTLMS = mcpevents.TTLNull()
			result := h.mustSubscribe(req)
			h.clock.Advance(30 * 24 * time.Hour)
			h.drain()
			row := h.row(result.ID)
			assert.Equal(t, db.MCPEventStateActive, row.State)
			assert.Zero(t, row.FailedAttempts)
		})
	})
	t.Run("policy refusal restarts failure aging", func(t *testing.T) {
		for _, priorAttempts := range []int{0, 4} {
			t.Run(strconv.Itoa(priorAttempts+1), func(t *testing.T) {
				forEachBackend(t, func(t *testing.T, store db.Storage) {
					h, sk := newDelivery(t, store)
					sk.respond = always(http.StatusInternalServerError)
					var blocked atomic.Bool
					h.recv.client.SetResolverForTest(func(context.Context, string, string) ([]netip.Addr, error) {
						if blocked.Load() {
							return []netip.Addr{netip.MustParseAddr("100.64.0.1")}, nil
						}
						return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
					})
					req := h.request("example-user")
					req.TTLMS = mcpevents.TTLNull()
					result := h.mustSubscribe(req)
					h.touch(result.ID)
					for range priorAttempts {
						h.drainDue()
						h.clock.Advance(10 * time.Minute)
					}
					seed(t, h, result.ID, 8*24*time.Hour, 19)
					blocked.Store(true)
					h.drainDue()
					row := h.row(result.ID)
					assert.Equal(t, "destination_not_allowed", row.LastError)
					assert.Equal(t, db.MCPEventStateActive, row.State)
					assert.Zero(t, row.FailedAttempts)
					assert.Nil(t, row.FailedSince)
					if priorAttempts == 4 {
						assert.Equal(t, 1, row.DeadLetterCount)
						h.touch(result.ID)
					} else {
						require.NotNil(t, row.NextAttemptAt)
					}
					h.clock.Advance(8 * 24 * time.Hour)
					blocked.Store(false)
					h.drainDue()
					row = h.row(result.ID)
					assert.Equal(t, db.MCPEventStateActive, row.State)
					assert.Equal(t, "http_5xx", row.LastError)
					assert.Equal(t, 1, row.FailedAttempts)
					require.NotNil(t, row.FailedSince)
					assert.Equal(t, h.clock.Now(), *row.FailedSince)
				})
			})
		}
	})
}

// A successful refresh clears endpoint failure aging even when a delivery that
// began under the old failure run has not returned yet.
func TestDeliveryRefreshDuringFailureStartsANewFailureRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timing  string
		attempt int
	}{
		{"first attempt during webhook", "during webhook", 1},
		{"first attempt before terminal write", "before terminal write", 1},
		{"last attempt during webhook", "during webhook", 5},
		{"last attempt before terminal write", "before terminal write", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				defer once.Do(func() { close(release) })
				block := func() { close(entered); <-release }
				h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) {
					if tc.timing == "before terminal write" {
						cfg.Store = terminalDeliveryGate{Storage: store, beforeEnd: block}
					}
				})
				req := h.request("example-user")
				result := h.mustSubscribe(req)
				row := h.row(result.ID)
				since := h.clock.Now().Add(-8 * 24 * time.Hour)
				row.FailedSince, row.FailedAttempts = &since, 19
				if tc.attempt == 5 {
					row.PendingKind = db.MCPEventPendingEvent
					row.PendingMessageID = "evt_last_attempt"
					row.PendingBody = []byte(`{"data":{"recipient":"example-user"}}`)
					row.AttemptCount = tc.attempt - 1
				}
				require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))

				sk.respond = func(n int, _ hit) (int, http.Header) {
					if n == 1 && tc.timing == "during webhook" {
						block()
					}
					return http.StatusInternalServerError, nil
				}
				if tc.attempt == 1 {
					h.touch(result.ID)
				}
				done := make(chan error, 1)
				go func() { done <- h.svc.Drain(t.Context()) }()
				receive(t, entered)
				pending := h.row(result.ID)
				require.Equal(t, tc.attempt, pending.AttemptCount)
				refreshed := h.mustSubscribe(req)
				require.Equal(t, result.ID, refreshed.ID)
				reset := h.row(result.ID)
				require.Zero(t, reset.FailedAttempts)
				require.Nil(t, reset.FailedSince)
				require.Equal(t, pending.Generation, reset.Generation)
				require.Equal(t, pending.ClaimToken, reset.ClaimToken)
				once.Do(func() { close(release) })
				require.NoError(t, receive(t, done))

				after := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateActive, after.State)
				assert.Empty(t, after.EndReason)
				assert.Equal(t, 1, after.FailedAttempts, "the in-flight failure begins a new run")
				require.NotNil(t, after.FailedSince)
				assert.True(t, after.FailedSince.Equal(h.clock.Now()))
				if tc.attempt == 5 {
					assert.Empty(t, after.PendingKind)
					assert.Nil(t, after.NextAttemptAt)
					assert.Zero(t, after.AttemptCount)
					assert.Equal(t, 1, after.DeadLetterCount)
				} else {
					assert.Equal(t, pending.PendingMessageID, after.PendingMessageID)
					assert.Equal(t, pending.PendingBody, after.PendingBody)
					assert.Equal(t, tc.attempt, after.AttemptCount)
					require.NotNil(t, after.NextAttemptAt)
					assert.True(t, after.NextAttemptAt.Equal(h.clock.Now().Add(30*time.Second)))
				}
				assert.Empty(t, after.ClaimToken)
				assert.Equal(t, 1, sk.count(), "no terminated control follows the refresh")
			})
		})
	}
}

// terminalDeliveryGate pauses a real conditional store write before execution.
type terminalDeliveryGate struct {
	db.Storage
	beforeEnd     func()
	endReason     string
	beforeRefresh func()
}

func (s terminalDeliveryGate) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	reason := s.endReason
	if reason == "" {
		reason = "endpoint_dead"
	}
	if in.EndReason != nil && *in.EndReason == reason {
		s.beforeEnd()
	}
	return s.Storage.UpdateMCPEventDelivery(ctx, in)
}

func (s terminalDeliveryGate) RefreshMCPEventSubscription(ctx context.Context, in db.MCPEventRefresh) (db.MCPEventSubscription, error) {
	if s.beforeRefresh != nil {
		s.beforeRefresh()
	}
	return s.Storage.RefreshMCPEventSubscription(ctx, in)
}

// A refresh started before expiry can commit after a worker has decided to
// expire its stale snapshot. The write must recheck the persisted lifetime.
// The worker reaches an expired row only when it has a change to stage or a
// ping to retry; the sweep expires clean rows in one conditional statement.
func TestDeliveryRefreshWinsBeforeExpiryWrite(t *testing.T) {
	for _, kind := range []string{"dirty", "event"} {
		t.Run(kind, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				refreshEntered, refreshRelease := make(chan struct{}), make(chan struct{})
				endEntered, endRelease := make(chan struct{}), make(chan struct{})
				var refreshOnce, endOnce sync.Once
				defer refreshOnce.Do(func() { close(refreshRelease) })
				defer endOnce.Do(func() { close(endRelease) })
				h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) {
					cfg.Store = terminalDeliveryGate{
						Storage: store, endReason: "expired",
						beforeRefresh: func() { close(refreshEntered); <-refreshRelease },
						beforeEnd:     func() { close(endEntered); <-endRelease },
					}
				})
				req := h.request("example-user")
				req.TTLMS = mcpevents.TTLValue((2 * time.Minute).Milliseconds())
				result := h.mustSubscribe(req)
				h.touch(result.ID)
				if kind == "event" {
					sk.respond = always(http.StatusInternalServerError)
					h.drain()
					require.Equal(t, db.MCPEventPendingEvent, h.row(result.ID).PendingKind)
					sk.respond = nil
				}

				h.clock.Advance(119 * time.Second)
				refreshDone := make(chan error, 1)
				go func() { _, err := h.subscribe(principalA, req); refreshDone <- err }()
				receive(t, refreshEntered)
				h.clock.Advance(time.Second)
				done := make(chan error, 1)
				go func() { done <- h.svc.Drain(t.Context()) }()
				receive(t, endEntered)
				before := h.row(result.ID)
				require.NotEmpty(t, before.ClaimToken)
				refreshOnce.Do(func() { close(refreshRelease) })
				require.NoError(t, receive(t, refreshDone))
				refreshed := h.row(result.ID)
				require.Equal(t, before.Generation, refreshed.Generation)
				require.Equal(t, before.ClaimToken, refreshed.ClaimToken)
				require.True(t, refreshed.ExpiresAt.After(h.clock.Now()))
				endOnce.Do(func() { close(endRelease) })
				require.NoError(t, receive(t, done))

				after := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateActive, after.State)
				assert.Empty(t, after.EndReason)
				assert.Nil(t, after.EndedAt)
				assert.Equal(t, before.Generation, after.Generation)
				assert.Empty(t, after.PendingKind)
				assert.Empty(t, after.ClaimToken)
				if kind == "event" {
					require.Len(t, sk.all(), 2, "the refreshed pending item is retried")
					assert.Equal(t, sk.all()[0].id, sk.all()[1].id)
				} else {
					require.Len(t, sk.all(), 1, "the change is still delivered")
				}
				count := sk.count()
				h.touch(result.ID)
				h.drain()
				assert.Equal(t, count+1, sk.count(), "later notifications still deliver")
			})
		})
	}
}

func TestDeliveryTerminatesAfterRevocationEvenThoughThePrincipalIsGone(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		result := h.mustSubscribe(req)
		h.auth.fail(mcpevents.ErrPrincipalRevoked)

		h.drain()

		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "principal_revoked", row.EndReason)
		assert.Empty(t, row.PendingKind, "the notice was delivered and settled")
		hits := sk.all()
		require.Len(t, hits, 1)
		assert.Equal(t, "terminated", hits[0].typ)
		assert.True(t, strings.HasPrefix(hits[0].id, "msg_terminated_"))
		verifySignature(t, hits[0], testSecret(1))
		var body struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(hits[0].body, &body))
		assert.Equal(t, -32024, body.Error.Code)

		_, err := h.subscribe(principalA, req)
		require.ErrorIs(t, err, mcpevents.ErrForbidden, "a refresh stays forbidden while the cause holds")
		h.drain()
		assert.Equal(t, 1, sk.count(), "the notice is sent once")
	})
}

func TestDeliveryTerminatesWhenNoVisibleProjectRemains(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		req.Arguments.ProjectUIDs = []string{testProjectA}
		result := h.mustSubscribe(req)
		h.fake.setProjects()

		h.drain()

		row := h.row(result.ID)
		assert.Equal(t, "scope_removed", row.EndReason)
		require.Equal(t, 1, sk.count())
		assert.Equal(t, "terminated", sk.all()[0].typ)
	})
}

func TestDeliveryReactivationClearsAnUnsentTerminatedNotice(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		req := h.request("example-user")
		result := h.mustSubscribe(req)
		h.auth.fail(mcpevents.ErrPrincipalRevoked)
		h.drain()
		ended := h.row(result.ID)
		require.Equal(t, db.MCPEventPendingTerminated, ended.PendingKind)
		require.Equal(t, 1, sk.count())

		h.auth.fail(nil)
		again := h.mustSubscribe(req)

		row := h.row(again.ID)
		assert.Equal(t, db.MCPEventStateActive, row.State)
		assert.Empty(t, row.PendingKind)
		assert.Equal(t, ended.Generation+1, row.Generation)
		h.clock.Advance(time.Hour)
		h.drain()
		assert.Equal(t, 1, sk.count(), "the cleared notice is never sent into the new generation")
	})
}

func TestDeliveryNeverSendsAnEventAfterExpiry(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		req := h.request("example-user")
		req.TTLMS = mcpevents.TTLValue((2 * time.Minute).Milliseconds())
		result := h.mustSubscribe(req)
		h.touch(result.ID)
		h.drain()
		require.Equal(t, 1, sk.count())

		// No sweep runs: only the due retry looks at the row.
		h.clock.Advance(3 * time.Minute)
		h.drainDue()

		assert.Equal(t, 1, sk.count(), "an expired grant is never sent to")
		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "expired", row.EndReason)
		assert.Empty(t, row.PendingKind, "no terminated notice follows an expiry")
	})
}

// A pending gap must not outlive the authority that admitted it.

// A pending ping must not outlive the authority that admitted it.
func TestDeliveryNeverRetriesAPingAfterAuthorityLoss(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		for _, tc := range []struct {
			name string
			err  error
		}{
			{"principal_revoked", mcpevents.ErrPrincipalRevoked},
			{"selection_removed", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h, sk := newDelivery(t, open(t))
				sk.respond = always(http.StatusInternalServerError)
				req := h.request("example-user")
				req.Arguments.ProjectUIDs = []string{testProjectA}
				result := h.mustSubscribe(req)
				h.touch(result.ID)
				h.drain()
				require.Equal(t, 1, sk.count())
				require.Equal(t, db.MCPEventPendingEvent, h.row(result.ID).PendingKind)

				if tc.err != nil {
					h.auth.fail(tc.err)
				} else {
					h.fake.setProjects(mcpevents.Project{UID: testProjectB, ID: 2})
				}
				h.clock.Advance(time.Minute)
				h.drainDue()

				row := h.row(result.ID)
				require.Equal(t, db.MCPEventStateEnded, row.State)
				reason := tc.name
				if tc.name == "selection_removed" {
					reason = "scope_removed"
				}
				assert.Equal(t, reason, row.EndReason)
				require.Equal(t, db.MCPEventPendingTerminated, row.PendingKind)
				hits := sk.all()
				require.Len(t, hits, 2)
				assert.Equal(t, "terminated", hits[1].typ, "the ping is not sent again")

				// Termination retries remain deliverable after authority loss.
				sk.respond = always(http.StatusOK)
				h.clock.Advance(time.Minute)
				h.drainDue()
				assert.Empty(t, h.row(result.ID).PendingKind)
				require.Equal(t, 3, sk.count())
				assert.Equal(t, "terminated", sk.all()[2].typ)
			})
		}
	})
}

// A subscription with nothing to deliver still ends within a few seconds of
// its owner losing authority.
func TestDeliveryRechecksIdleSubscriptionsEveryFewSeconds(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.drain()
		h.auth.fail(mcpevents.ErrPrincipalRevoked)

		h.drainDue()
		assert.Equal(t, db.MCPEventStateActive, h.row(result.ID).State, "rechecks are rate limited")

		h.clock.Advance(5 * time.Second)
		h.drainDue()
		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "principal_revoked", row.EndReason)
		require.Equal(t, 1, sk.count())
		assert.Equal(t, "terminated", sk.all()[0].typ)
	})
}

func TestSweepEndsExpiredIdleSubscriptions(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		req.TTLMS = mcpevents.TTLValue((2 * time.Minute).Milliseconds())
		result := h.mustSubscribe(req)
		h.clock.Advance(3 * time.Minute)
		require.NoError(t, h.svc.Sweep(t.Context()))
		h.drainDue()
		assert.Equal(t, "expired", h.row(result.ID).EndReason)
		assert.Zero(t, sk.count())
	})
}

func TestSweepPrunesEndedRowsWithTheRetentionCaps(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, _ := newDelivery(t, store)
		base := h.mustSubscribe(h.request("example-user"))
		template := h.row(base.ID)
		ended := h.clock.Now()
		insert := func(i int) {
			row := template
			row.ID = db.MCPEventSubscriptionIDPrefix + strconv.Itoa(1000+i)
			row.CallbackURL = testCallback + "&n=" + strconv.Itoa(i)
			row.State, row.EndReason, row.EndedAt = db.MCPEventStateEnded, "unsubscribed", &ended
			require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
		}
		for i := range 260 {
			insert(i)
		}
		require.NoError(t, h.svc.Sweep(t.Context()))
		rows, err := h.store.ListMCPEventSubscriptions(t.Context(), principalA.Key())
		require.NoError(t, err)
		assert.Len(t, rows, 256+1, "256 ended rows per principal survive, plus the active one")
	})
}

func TestDeliveryAuthorityOutageKeepsTheRowAndRetriesLater(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.touch(result.ID)
		h.auth.fail(mcpevents.ErrAuthorityUnavailable)
		h.drain()
		assert.Zero(t, sk.count())
		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateActive, row.State)
		assert.Empty(t, row.ClaimToken)

		h.auth.fail(nil)
		h.clock.Advance(time.Minute)
		h.drain()
		assert.Equal(t, 1, sk.count())
	})
}

func TestDeliveryEndsRowsWhoseStoredStateCannotBeRead(t *testing.T) {
	for name, damage := range map[string]func(*db.MCPEventSubscription){
		"secret":    func(r *db.MCPEventSubscription) { r.SecretEnc = []byte("not a sealed secret") },
		"arguments": func(r *db.MCPEventSubscription) { r.Arguments = []byte("not json") },
	} {
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				h, sk := newDelivery(t, store)
				result := h.mustSubscribe(h.request("example-user"))
				row := h.row(result.ID)
				damage(&row)
				require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
				h.touch(result.ID)

				h.drain()
				h.clock.Advance(time.Minute)
				h.drain()

				assert.Zero(t, sk.count())
				got := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateEnded, got.State, "no endless retry")
				assert.Equal(t, "endpoint_dead", got.EndReason)
				assert.Empty(t, got.PendingKind)
				assert.Empty(t, got.ClaimToken)
			})
		})
	}
}

// A host can rename the actor behind one principal, and subscriptions made
// before and after the rename keep their own actor. Authority is decided per
// stored actor, so losing it under the old name ends only the old rows.
func TestDeliveryRechecksEachStoredActorSeparately(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, _ := newDelivery(t, store)
		old := h.mustSubscribe(h.request("old-recipient"))
		renamed := principalA
		renamed.Actor = "renamed-user"
		current, err := h.subscribe(renamed, h.request("new-recipient"))
		require.NoError(t, err)
		require.Equal(t, h.row(old.ID).PrincipalKey, h.row(current.ID).PrincipalKey, "one principal")

		h.auth.mu.Lock()
		h.auth.revokedActor = "example-user"
		h.auth.mu.Unlock()
		h.drain()

		assert.Equal(t, "principal_revoked", h.row(old.ID).EndReason)
		assert.Equal(t, db.MCPEventStateActive, h.row(current.ID).State, "the renamed actor keeps its authority")
	})
}

// renameGate refreshes a subscription under a renamed actor just before an
// authority-driven end runs, as a host rename racing the check would.
type renameGate struct {
	db.Storage
	once   sync.Once
	rename func()
}

func (g *renameGate) EndMCPEventSubscription(ctx context.Context, in db.MCPEventEnd) (db.MCPEventSubscription, error) {
	if in.Reason == "principal_revoked" {
		g.once.Do(g.rename)
	}
	return g.Storage.EndMCPEventSubscription(ctx, in)
}

func (g *renameGate) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	if in.EndReason != nil && *in.EndReason == "principal_revoked" {
		g.once.Do(g.rename)
	}
	return g.Storage.UpdateMCPEventDelivery(ctx, in)
}

// A denial for the actor that was checked must not end a row a concurrent
// refresh has moved to a renamed actor. The authority recheck leaves it for
// its next pass; delivery re-checks under the new actor and sends.
func TestAuthorityEndIsFencedAgainstAnActorRename(t *testing.T) {
	renamed := principalA
	renamed.Actor = "renamed-user"
	for _, path := range []string{"recheck", "delivery"} {
		t.Run(path, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, store db.Storage) {
				gate := &renameGate{Storage: store}
				h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.Store = gate })
				req := h.request("example-user")
				result := h.mustSubscribe(req)
				gate.rename = func() {
					_, err := h.subscribe(renamed, req)
					require.NoError(t, err)
				}
				h.drain()
				if path == "delivery" {
					// A ping due now, with the authority recheck already run
					// at this time, so delivery makes the decision.
					row := h.row(result.ID)
					row.PendingKind, row.PendingMessageID = db.MCPEventPendingEvent, "evt_retry"
					row.PendingBody, row.AttemptCount = []byte(`{"eventId":"evt_retry"}`), 1
					require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
				}
				h.auth.mu.Lock()
				h.auth.revokedActor = "example-user"
				h.auth.mu.Unlock()
				if path == "recheck" {
					h.drain()
				} else {
					h.drainDue()
				}

				row := h.row(result.ID)
				assert.Equal(t, db.MCPEventStateActive, row.State, "the refreshed row keeps its grant")
				assert.Equal(t, "renamed-user", row.Actor)
				for _, got := range sk.all() {
					assert.NotEqual(t, "terminated", got.typ)
				}
				if path == "delivery" {
					require.Len(t, sk.all(), 1, "the ping is sent under the renamed actor")
					assert.Equal(t, "evt_retry", sk.all()[0].id)
				}
				h.drain()
				assert.Equal(t, db.MCPEventStateActive, h.row(result.ID).State, "a later recheck under the renamed actor keeps it")
			})
		})
	}
}

// advanceGate runs a hook once, when the scan publishes a position at or past
// a given event: after its matching, before the position is visible.
type advanceGate struct {
	db.Storage
	past int64
	once sync.Once
	hook func()
}

func (g *advanceGate) AdvanceMCPEventsScanPosition(ctx context.Context, from, to int64) (bool, error) {
	if to >= g.past && g.hook != nil {
		g.once.Do(g.hook)
	}
	return g.Storage.AdvanceMCPEventsScanPosition(ctx, from, to)
}

// insertPulledRequest records a pulled issue.created that asks example-user
// for work, as a federation pull commits it before applying it.
func insertPulledRequest(t *testing.T, store db.Storage, project db.Project, eventUID, issueUID, origin string) int64 {
	t.Helper()
	execer, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	payload := `{"title":"pulled","metadata":{"` + notification.MetadataKey("example-user") + `":{"from":"coordinator","message":"review"}}}`
	_, err := execer.ExecContext(t.Context(), `INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		issue_uid, type, actor, payload, hlc_physical_ms, hlc_counter, content_hash)
		VALUES($1, $2, $3, $4, $5, 'issue.created', 'coordinator', $6, 1, 0, $7)`,
		eventUID, origin, project.ID, project.Name, issueUID, payload, strings.Repeat("a", 64))
	require.NoError(t, err)
	id, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	return id
}

// recordApplied records applied pulled events as a pull's applying
// transaction does.
func recordApplied(t *testing.T, store db.Storage, eventUIDs ...string) {
	t.Helper()
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok)
	tx, err := beginner.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, db.RecordMCPEventsRematch(t.Context(), tx, eventUIDs))
	require.NoError(t, tx.Commit())
}

// One worker skips a pulled event whose issue is not applied yet and has not
// published its new position when the pull applies the event. Another
// worker's rematch then sees the event beyond the published position. It may
// acknowledge only the events it matched, or the first worker moves past the
// event and nobody is pinged.
func TestRematchKeepsAppliedEventsBeyondThePublishedPosition(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "pulled-project")
		require.NoError(t, err)
		gate := &advanceGate{Storage: store}
		h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.Store = gate })
		h.fake.setProjects(mcpevents.Project{UID: project.UID, ID: project.ID, Name: project.Name})
		_, earlier, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "earlier", Author: "coordinator"})
		require.NoError(t, err)
		h.mustSubscribe(h.request("example-user"))
		h.drain()

		const eventUID, issueUID = "01ARZ3NDEKTSV4RRFFQ69G5FF1", "01ARZ3NDEKTSV4RRFFQ69G5FF2"
		h.fake.mu.Lock()
		h.fake.hiddenIssue = map[string]bool{issueUID: true} // not applied yet
		h.fake.mu.Unlock()
		gate.past = insertPulledRequest(t, store, project, eventUID, issueUID, "01ARZ3NDEKTSV4RRFFQ69G5FAW")
		gate.hook = func() {
			h.fake.mu.Lock()
			h.fake.hiddenIssue = nil // the pull applies it
			h.fake.mu.Unlock()
			// The same pull applied an event the scan had published.
			recordApplied(t, store, earlier.UID, eventUID)
			require.NoError(t, h.svc.Rematch(ctx))
		}

		h.drainDue()
		assert.Equal(t, 1, sk.count(), "the applied request is delivered")
	})
}

// Federation reset replay reinserts events that originated on this daemon,
// through a pull. Applying them records a rematch like any other pull, and
// their origin does not exclude them from it.
func TestRematchCoversReplayedEventsOfLocalOrigin(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		project, err := store.CreateProject(ctx, "replayed-project")
		require.NoError(t, err)
		h, sk := newDelivery(t, store)
		h.fake.setProjects(mcpevents.Project{UID: project.UID, ID: project.ID, Name: project.Name})
		h.mustSubscribe(h.request("example-user"))
		h.drain()

		const eventUID, issueUID = "01ARZ3NDEKTSV4RRFFQ69G5FF3", "01ARZ3NDEKTSV4RRFFQ69G5FF4"
		h.fake.mu.Lock()
		h.fake.hiddenIssue = map[string]bool{issueUID: true}
		h.fake.mu.Unlock()
		insertPulledRequest(t, store, project, eventUID, issueUID, store.InstanceUID())
		h.drainDue()
		require.Zero(t, sk.count(), "skipped before it is applied")

		h.fake.mu.Lock()
		h.fake.hiddenIssue = nil
		h.fake.mu.Unlock()
		recordApplied(t, store, eventUID)
		h.drainDue()
		assert.Equal(t, 1, sk.count())
	})
}

// catalogGate runs a hook once it is armed, when the scan stores catalog
// fingerprints: after it computed them, before they are written.
type catalogGate struct {
	db.Storage
	mu   sync.Mutex
	hook func()
}

func (g *catalogGate) UpdateMCPEventCatalogs(ctx context.Context, versions map[string]string, observedThrough int64) (bool, error) {
	g.mu.Lock()
	hook := g.hook
	g.hook = nil
	g.mu.Unlock()
	if hook != nil {
		hook()
	}
	return g.Storage.UpdateMCPEventCatalogs(ctx, versions, observedThrough)
}

// One worker computes fingerprints after an owner gains a project, and
// stores them after another worker saw that access revoked again. The stale
// fingerprint must not stand, or the owner's next gain of the project matches
// it and nobody is pinged.
func TestCatalogRefreshDropsFingerprintsComputedBeforeANewerChange(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		ctx := t.Context()
		gate := &catalogGate{Storage: store}
		h, sk := newDelivery(t, store, func(cfg *mcpevents.ServiceConfig) { cfg.Store = gate })
		other, err := mcpevents.NewService(ctx, mcpevents.ServiceConfig{
			Store: store, Keys: h.keys, Authority: h.auth, Webhook: h.recv.client, Now: h.clock.Now,
			Jitter: func() float64 { return 0 }, RenewInterval: time.Hour, PollInterval: time.Hour,
		})
		require.NoError(t, err)
		h.mustSubscribe(h.request("example-user"))
		h.drain()

		h.changeVisibility(visibleProjectA, visibleProjectB)
		gate.mu.Lock()
		gate.hook = func() {
			h.changeVisibility(visibleProjectA)
			require.NoError(t, other.Drain(ctx))
		}
		gate.mu.Unlock()
		h.drainDue()

		before := sk.count()
		h.changeVisibility(visibleProjectA, visibleProjectB)
		h.drainDue()
		assert.Equal(t, before+1, sk.count(), "gaining the project again pings the owner")
	})
}

var (
	visibleProjectA = mcpevents.Project{UID: testProjectA, ID: 1, Name: "hub-project"}
	visibleProjectB = mcpevents.Project{UID: testProjectB, ID: 2, Name: "spoke-project"}
)

// changeVisibility makes the owner see exactly the given projects, and
// creates a project so the archive records a change that can alter what
// owners see.
func (h *harness) changeVisibility(visible ...mcpevents.Project) {
	h.t.Helper()
	h.fake.setProjects(visible...)
	_, _, err := h.store.CreateProjectAndEvent(h.t.Context(), "catalog-"+strconv.FormatInt(h.clock.Now().UnixNano(), 10), "tester")
	require.NoError(h.t, err)
	h.clock.Advance(time.Minute)
}

// An owner gains a project while its subscription waits on callback
// verification, and the catch-up scan consumes that change before the row is
// active. The row must not keep the fingerprint from before the gain, or
// losing the project again matches it and nobody is pinged.
func TestActivationDoesNotKeepAFingerprintFromBeforeVerification(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		h.recv.gate, h.recv.entered = make(chan struct{}), make(chan struct{}, 1)
		subscribed := make(chan error, 1)
		go func() {
			_, err := h.subscribe(principalA, h.request("example-user"))
			subscribed <- err
		}()
		<-h.recv.entered
		h.changeVisibility(visibleProjectA, visibleProjectB)
		close(h.recv.gate)
		require.NoError(t, <-subscribed)
		h.drain()

		before := sk.count()
		h.changeVisibility(visibleProjectA)
		h.drainDue()
		assert.Equal(t, before+1, sk.count(), "losing the project pings the owner")
	})
}

// An owner gains a project while its authority cannot be checked. The
// deferred ping goes out once authority returns, but the stored fingerprint
// is still the one from before the gain; losing the project again must not
// match it.
func TestAuthorityRecoveryDoesNotKeepAFingerprintFromBeforeTheOutage(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		h.mustSubscribe(h.request("example-user"))
		h.drain()

		h.auth.fail(mcpevents.ErrAuthorityUnavailable)
		h.changeVisibility(visibleProjectA, visibleProjectB)
		h.drainDue()
		h.auth.fail(nil)
		h.clock.Advance(time.Minute)
		h.drainDue()
		before := sk.count()
		require.Positive(t, before, "the deferred ping goes out after recovery")

		h.changeVisibility(visibleProjectA)
		h.drainDue()
		assert.Equal(t, before+1, sk.count(), "losing the project pings the owner")
	})
}
