package mcpevents_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/mcpevents"
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

func (h *harness) event(id int64) mcpevents.Occurrence {
	return notifyOccurrence(id, testProjectA, "example-user", h.clock.Now())
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

func TestDeliverySettlesTheCursorOn2xx(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		occurrence := h.event(101)
		h.fake.addEvent(occurrence)

		h.drain()

		hits := sk.all()
		require.Len(t, hits, 1)
		first := hits[0]
		assert.Equal(t, mcpevents.InboxSourceEventID(h.store.InstanceUID(), occurrence.EventUID, "example-user"), first.id)
		assert.Equal(t, result.ID, first.subscription)
		assert.Equal(t, "application/json", first.contentType)
		verifySignature(t, first, testSecret(1))
		var envelope mcpevents.Envelope
		require.NoError(t, json.Unmarshal(first.body, &envelope))
		assert.Equal(t, first.id, envelope.EventID)
		assert.Equal(t, mcpevents.EventName, envelope.Name)
		assert.Equal(t, "notification", envelope.Data.Reason)
		assert.Equal(t, "example-user", envelope.Data.Recipient)
		assert.Equal(t, testProjectA, envelope.Data.ProjectUID)
		cursor, err := mcpevents.DecodeCursor(h.key, result.ID, envelope.Cursor)
		require.NoError(t, err)
		assert.Equal(t, int64(101), cursor.EventID)

		row := h.row(result.ID)
		assert.Equal(t, int64(101), row.CursorEventID)
		assert.Empty(t, row.PendingKind)
		assert.Zero(t, row.AttemptCount)
		assert.Nil(t, row.NextAttemptAt, "a settle leaves nothing hidden from the due list")
		assert.Empty(t, row.ClaimToken)
		require.NotNil(t, row.LastDeliveryAt)
		assert.Zero(t, row.FailedAttempts)
	})
}

func TestDeliverySignsWithBothKeysDuringARotationWindow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		req.Delivery.Secret = testSecret(2)
		h.mustSubscribe(req)
		h.fake.addEvent(h.event(101))
		h.drain()
		h.fake.addEvent(h.event(102))
		h.clock.Advance(61 * time.Second)
		h.drain()

		hits := sk.all()
		require.Len(t, hits, 2)
		verifySignature(t, hits[0], testSecret(2), testSecret(1))
		verifySignature(t, hits[1], testSecret(2))
		assert.Equal(t, first.ID, hits[1].subscription)
	})
}

func TestDelivery410And413DropOneOccurrenceOnly(t *testing.T) {
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
				h.fake.addEvent(h.event(101))
				h.fake.addEvent(h.event(102))

				h.drain()

				require.Equal(t, 2, sk.count())
				row := h.row(result.ID)
				assert.Equal(t, 1, row.DeadLetterCount)
				assert.Equal(t, int64(102), row.CursorEventID)
				assert.Equal(t, db.MCPEventStateActive, row.State)
				assert.Nil(t, row.NextAttemptAt)
				assert.Empty(t, row.PendingKind)
			})
		})
	}
}

func TestDeliveryRetriesOnTheFixedSchedule(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		result := h.mustSubscribe(h.request("example-user"))
		h.fake.addEvent(h.event(101))
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
		assert.Equal(t, int64(101), row.CursorEventID)
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
		h.fake.addEvent(h.event(101))
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
		h.fake.addEvent(h.event(101))
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
			h.fake.addEvent(h.event(101))
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
			h.fake.addEvent(h.event(101))
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
			h.fake.addEvent(h.event(101))
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
					next := int64(101)
					row.PendingKind = db.MCPEventPendingEvent
					row.PendingMessageID = "evt-last-attempt"
					row.PendingBody = []byte(`{"data":{"project_uid":"` + testProjectA + `","issue_uid":"` + testIssue + `"}}`)
					row.PendingNextCursor, row.AttemptCount = &next, tc.attempt-1
				}
				require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))

				sk.respond = func(n int, _ hit) (int, http.Header) {
					if n == 1 && tc.timing == "during webhook" {
						block()
					}
					return http.StatusInternalServerError, nil
				}
				h.fake.addEvent(h.event(101))
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
					assert.Equal(t, int64(101), after.CursorEventID)
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
	beforeEnd func()
}

func (s terminalDeliveryGate) UpdateMCPEventDelivery(ctx context.Context, in db.MCPEventDeliveryUpdate) (bool, error) {
	if in.EndReason != nil && *in.EndReason == "endpoint_dead" {
		s.beforeEnd()
	}
	return s.Storage.UpdateMCPEventDelivery(ctx, in)
}

func TestDeliveryReportsAPurgeGapWithTheHeadCursor(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		before := h.row(first.ID)
		h.fake.reset(150, 150)

		h.drain()

		row := h.row(first.ID)
		assert.Equal(t, int64(150), row.CursorEventID)
		assert.Equal(t, before.ContinuityGeneration+1, row.ContinuityGeneration)
		assert.True(t, row.GapUnacked)
		assert.Equal(t, before.Generation+1, row.Generation, "a gap fences everything outstanding")
		hits := sk.all()
		require.Len(t, hits, 1)
		assert.Equal(t, "gap", hits[0].typ)
		assert.True(t, strings.HasPrefix(hits[0].id, "msg_gap_"))
		verifySignature(t, hits[0], testSecret(1))
		var body struct {
			Cursor string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(hits[0].body, &body))
		cursor, err := mcpevents.DecodeCursor(h.key, first.ID, body.Cursor)
		require.NoError(t, err)
		assert.Equal(t, mcpevents.Cursor{Incarnation: row.Incarnation, ContinuityGeneration: row.ContinuityGeneration, EventID: 150}, cursor)

		// The control is retried like an event and then abandoned, while the
		// unacknowledged gap stays reported.
		for _, delay := range []time.Duration{30 * time.Second, time.Minute, 3 * time.Minute, 8 * time.Minute} {
			h.clock.Advance(delay)
			h.drainDue()
		}
		require.Equal(t, 5, sk.count())
		row = h.row(first.ID)
		assert.Empty(t, row.PendingKind)
		assert.Nil(t, row.NextAttemptAt)
		assert.True(t, row.GapUnacked)

		stale := req
		stale.Cursor = &first.Cursor
		refreshed, err := h.subscribe(principalA, stale)
		require.NoError(t, err)
		assert.True(t, refreshed.Truncated, "a refresh still reports the unacknowledged gap")
		current, err := mcpevents.DecodeCursor(h.key, first.ID, refreshed.Cursor)
		require.NoError(t, err)
		assert.Equal(t, row.ContinuityGeneration, current.ContinuityGeneration)

		stale.Cursor = &refreshed.Cursor
		cleared, err := h.subscribe(principalA, stale)
		require.NoError(t, err)
		assert.False(t, cleared.Truncated, "a current-generation cursor proves the client learned of the gap")
		assert.False(t, h.row(first.ID).GapUnacked)
	})
}

func TestDeliveryAcknowledgedGapClearsTheFlag(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		first := h.mustSubscribe(h.request("example-user"))
		h.fake.reset(150, 150)
		h.drain()
		require.Equal(t, 1, sk.count())
		row := h.row(first.ID)
		assert.False(t, row.GapUnacked)
		assert.Empty(t, row.PendingKind)
		assert.Equal(t, int64(150), row.CursorEventID)
	})
}

// A maxAgeMs skip found during delivery is an in-place gap: the control
// carries the settled position, never the head, and eligible replay continues.
func TestDeliveryMaxAgeSkipIsAnInPlaceGap(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = func(n int, _ hit) (int, http.Header) {
			if n == 1 {
				return http.StatusInternalServerError, nil
			}
			return http.StatusOK, nil
		}
		first := h.mustSubscribe(h.request("example-user"))
		now := h.clock.Now()
		row := h.row(first.ID)
		cutoff, replayHead := now.Add(-time.Hour), int64(102)
		row.ReplayCutoff, row.ReplayHead = &cutoff, &replayHead
		require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
		h.fake.addEvent(notifyOccurrence(101, testProjectA, "example-user", now.Add(-2*time.Hour)))
		h.fake.addEvent(notifyOccurrence(102, testProjectA, "example-user", now))

		h.drain()

		hits := sk.all()
		require.Len(t, hits, 1)
		require.Equal(t, "gap", hits[0].typ)
		var body struct {
			Cursor string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(hits[0].body, &body))
		cursor, err := mcpevents.DecodeCursor(h.key, first.ID, body.Cursor)
		require.NoError(t, err)
		assert.Equal(t, int64(100), cursor.EventID, "the settled position, not the head")
		assert.Equal(t, row.ContinuityGeneration+1, cursor.ContinuityGeneration)
		pending := h.row(first.ID)
		require.NotNil(t, pending.PendingNextCursor)
		assert.Equal(t, int64(100), *pending.PendingNextCursor, "the control never checkpoints past undelivered replay")
		assert.True(t, pending.GapUnacked)

		h.clock.Advance(30 * time.Second)
		h.drainDue()

		hits = sk.all()
		require.Len(t, hits, 3, "gap, gap retry, then the eligible occurrence")
		assert.Equal(t, "gap", hits[1].typ)
		assert.Equal(t, hits[0].id, hits[1].id)
		assert.NotEqual(t, "gap", hits[2].typ)
		var envelope mcpevents.Envelope
		require.NoError(t, json.Unmarshal(hits[2].body, &envelope))
		assert.Equal(t, mcpevents.InboxSourceEventID(h.store.InstanceUID(), "01ARZ3NDEKTSV4RRFFQ"+mcpevents.PadID(102), "example-user"), envelope.EventID)
		done := h.row(first.ID)
		assert.Equal(t, int64(102), done.CursorEventID)
		assert.Nil(t, done.ReplayCutoff, "the replay bound clears once the cursor passes it")
		assert.Nil(t, done.ReplayHead)
		assert.False(t, done.GapUnacked)
	})
}

// A skip already signalled by the subscribe response queues no second control.
func TestDeliveryDoesNotDuplicateASkipSignalledBySubscribe(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		require.NoError(t, h.unsubscribe(req))
		now := h.clock.Now()
		h.fake.addEvent(notifyOccurrence(101, testProjectA, "example-user", now.Add(-2*time.Hour)))
		h.fake.addEvent(notifyOccurrence(102, testProjectA, "example-user", now))
		req.Cursor = &first.Cursor
		maxAge := (time.Hour).Milliseconds()
		req.MaxAgeMS = &maxAge
		again, err := h.subscribe(principalA, req)
		require.NoError(t, err)
		require.True(t, again.Truncated, "the response carries the signal")

		h.drain()

		hits := sk.all()
		require.Len(t, hits, 1, "only the eligible occurrence is sent")
		assert.NotEqual(t, "gap", hits[0].typ)
		assert.Equal(t, int64(102), h.row(first.ID).CursorEventID)
	})
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
		h.fake.addEvent(h.event(101))
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

func TestDeliveryNeverRetriesAGapAfterExpiry(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		req := h.request("example-user")
		req.TTLMS = mcpevents.TTLValue((2 * time.Minute).Milliseconds())
		result := h.mustSubscribe(req)
		h.fake.reset(150, 150)
		h.drain()
		require.Equal(t, 1, sk.count())
		require.Equal(t, "gap", sk.all()[0].typ)
		require.Equal(t, db.MCPEventPendingGap, h.row(result.ID).PendingKind)

		// Only the pending retry runs; no expiry sweep can clear it first.
		h.clock.Advance(3 * time.Minute)
		h.drainDue()

		assert.Equal(t, 1, sk.count(), "an expired grant receives no gap retry")
		row := h.row(result.ID)
		assert.Equal(t, db.MCPEventStateEnded, row.State)
		assert.Equal(t, "expired", row.EndReason)
		assert.Empty(t, row.PendingKind, "expiry sends no terminated notice")
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

func TestDeliveryReauthorizesDepartureSideMoves(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		req := h.request("example-user")
		req.Arguments.ProjectUIDs = []string{testProjectA}
		result := h.mustSubscribe(req)
		payload, err := json.Marshal(map[string]string{"from_project_uid": testProjectA, "to_project_uid": testProjectB})
		require.NoError(t, err)
		h.fake.addEvent(mcpevents.Occurrence{
			EventID: 101, EventUID: "01ARZ3NDEKTSV4RRFFQ" + mcpevents.PadID(101), Type: "issue.moved",
			ProjectUID: testProjectB, IssueUID: testIssue, Payload: payload, CreatedAt: h.clock.Now(),
		})

		h.drain()

		require.Equal(t, 1, sk.count(), "subscribers of the departure project still get the move")
		var envelope mcpevents.Envelope
		require.NoError(t, json.Unmarshal(sk.all()[0].body, &envelope))
		assert.Equal(t, "visibility", envelope.Data.Reason)
		assert.Equal(t, testProjectA, envelope.Data.ProjectUID)
		assert.Empty(t, envelope.Data.IssueUID, "the issue is not exposed outside the destination project")
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		require.NotEmpty(t, h.fake.authorized)
		last := h.fake.authorized[len(h.fake.authorized)-1]
		assert.Equal(t, mcpevents.Occurrence{ProjectUID: testProjectA}, last, "re-authorized against the from-project with no issue lookup")
		assert.Equal(t, int64(101), h.row(result.ID).CursorEventID)
	})
}

func TestDeliverySettlesAHiddenOccurrenceWithoutSending(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.fake.hiddenIssue = map[string]bool{testIssue: true}
		h.fake.addEvent(h.event(101))
		h.drain()
		assert.Zero(t, sk.count())
		row := h.row(result.ID)
		assert.Equal(t, int64(101), row.CursorEventID)
		assert.Empty(t, row.PendingKind)
	})
}

// stuckAuthorized models a scoped read whose Next only advances with events.
type stuckAuthorized struct{ *fakeAuthorized }

func (s stuckAuthorized) Read(_ context.Context, _ []mcpevents.Project, after int64, _ int) (mcpevents.Page, error) {
	return mcpevents.Page{Next: after}, nil
}

func TestDeliveryDoesNotSpinOnAScopedReadThatCannotAdvance(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.auth.setAuthorized(stuckAuthorized{h.fake})
		h.fake.addEvent(h.event(101))

		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		h.svc.Recheck()
		require.NoError(t, h.svc.Drain(ctx))

		assert.Zero(t, sk.count())
		row := h.row(result.ID)
		assert.Equal(t, int64(100), row.CursorEventID)
		assert.Empty(t, row.ClaimToken, "the claim is released")
		before := h.auth.calls.Load()
		require.NoError(t, h.svc.Drain(ctx))
		assert.Equal(t, before, h.auth.calls.Load(), "an unchanged archive is not rescanned until the next recheck")
	})
}

func TestDeliverySkipsIdleRowsUntilTheArchiveMovesOrTheMinuteSweep(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, _ := newDelivery(t, store)
		h.mustSubscribe(h.request("example-user"))
		h.drain()
		calls := h.auth.calls.Load()
		h.drainDue()
		h.drainDue()
		assert.Equal(t, calls, h.auth.calls.Load())

		h.clock.Advance(time.Minute + time.Second)
		require.NoError(t, h.svc.Sweep(t.Context()))
		h.drainDue()
		assert.Greater(t, h.auth.calls.Load(), calls, "the minute sweep revalidates idle subscriptions")
	})
}

func TestDeliveryAuthorityOutageKeepsTheRowAndRetriesLater(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		h.fake.addEvent(h.event(101))
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

// A replay of many stale occurrences costs one gap control and one continuity
// bump, and the first eligible occurrence after them is still delivered.
func TestDeliveryMaxAgeRunOfStaleOccurrencesIsOneGap(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		first := h.mustSubscribe(h.request("example-user"))
		now := h.clock.Now()
		row := h.row(first.ID)
		cutoff, replayHead := now.Add(-time.Hour), int64(104)
		row.ReplayCutoff, row.ReplayHead = &cutoff, &replayHead
		require.NoError(t, h.store.ImportMCPEventSubscription(t.Context(), row))
		for id := int64(101); id <= 103; id++ {
			h.fake.addEvent(notifyOccurrence(id, testProjectA, "example-user", now.Add(-2*time.Hour)))
		}
		h.fake.addEvent(notifyOccurrence(104, testProjectA, "example-user", now))

		h.drain()

		hits := sk.all()
		require.Len(t, hits, 2, "one gap control, then the eligible occurrence")
		assert.Equal(t, "gap", hits[0].typ)
		assert.NotEqual(t, "gap", hits[1].typ)
		var body struct {
			Cursor string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(hits[0].body, &body))
		cursor, err := mcpevents.DecodeCursor(h.key, first.ID, body.Cursor)
		require.NoError(t, err)
		assert.Equal(t, int64(100), cursor.EventID)
		done := h.row(first.ID)
		assert.Equal(t, row.ContinuityGeneration+1, done.ContinuityGeneration, "exactly one continuity bump")
		assert.Equal(t, int64(104), done.CursorEventID)
		assert.False(t, done.GapUnacked)
	})
}

// A retry for an issue that became hidden between attempts is never sent; the
// occurrence is dropped and the cursor settles so later ones still deliver.
func TestDeliveryRetryDropsAnOccurrenceHiddenInTheMeantime(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h, sk := newDelivery(t, store)
		sk.respond = always(http.StatusInternalServerError)
		result := h.mustSubscribe(h.request("example-user"))
		h.fake.addEvent(h.event(101))
		h.drain()
		require.Equal(t, 1, sk.count())

		h.fake.mu.Lock()
		h.fake.hiddenIssue = map[string]bool{testIssue: true}
		h.fake.mu.Unlock()
		h.clock.Advance(30 * time.Second)
		h.drainDue()

		assert.Equal(t, 1, sk.count(), "the hidden occurrence is not sent again")
		row := h.row(result.ID)
		assert.Equal(t, int64(101), row.CursorEventID)
		assert.Empty(t, row.PendingKind)
		assert.Nil(t, row.NextAttemptAt)
		assert.Zero(t, row.DeadLetterCount, "an unauthorized drop is not a delivery failure")
		assert.Equal(t, db.MCPEventStateActive, row.State)
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
				h.fake.addEvent(h.event(101))

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
