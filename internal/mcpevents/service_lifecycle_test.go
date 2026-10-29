package mcpevents_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

const otherIncarnation = "abababababababababababababababab"

func cursorString(h *harness, row db.MCPEventSubscription, incarnation string, continuity, eventID int64) *string {
	c := mcpevents.EncodeCursor(h.key, row.ID, mcpevents.Cursor{Incarnation: incarnation, ContinuityGeneration: continuity, EventID: eventID})
	return &c
}

// seedDelivery leaves the row mid-delivery: claimed, with a pending event, one
// attempt, a scheduled retry and an unacknowledged gap.
func seedDelivery(h *harness, id string) db.MCPEventSubscription {
	h.t.Helper()
	ctx := h.t.Context()
	row := h.row(id)
	now := h.clock.Now()
	ok, err := h.store.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: id, Incarnation: row.Incarnation, Generation: row.Generation, Token: "claim-token", Now: now, Until: now.Add(time.Minute)})
	require.NoError(h.t, err)
	require.True(h.t, ok)
	next, retryAt := int64(105), now.Add(30*time.Second)
	ok, err = h.store.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: id, Incarnation: row.Incarnation, Generation: row.Generation, ClaimToken: "claim-token", Now: now,
		Pending:          &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: "evt-1", Body: []byte(`{"eventId":"evt-1"}`), NextCursor: &next},
		IncrementAttempt: true, NextAttemptAt: &retryAt, BumpContinuity: true, GapUnacked: new(true),
	})
	require.NoError(h.t, err)
	require.True(h.t, ok)
	return h.row(id)
}

func TestLiveRefreshIsADeliveryNoOp(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		for _, tc := range []struct {
			name      string
			cursor    func(h *harness, row db.MCPEventSubscription) *string
			wantClear bool
		}{
			{"no cursor", func(*harness, db.MCPEventSubscription) *string { return nil }, false},
			{"current continuity cursor", func(h *harness, r db.MCPEventSubscription) *string {
				return cursorString(h, r, r.Incarnation, r.ContinuityGeneration, 103)
			}, true},
			{"older continuity cursor", func(h *harness, r db.MCPEventSubscription) *string {
				return cursorString(h, r, r.Incarnation, r.ContinuityGeneration-1, 103)
			}, false},
			{"older incarnation cursor", func(h *harness, r db.MCPEventSubscription) *string {
				return cursorString(h, r, otherIncarnation, 0, 40)
			}, false},
			{"cursor behind the settled position", func(h *harness, r db.MCPEventSubscription) *string {
				return cursorString(h, r, r.Incarnation, 0, 3)
			}, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t, open(t))
				req := h.request("example-user/" + time.Now().Format("150405.000000000"))
				first := h.mustSubscribe(req)
				before := seedDelivery(h, first.ID)
				require.True(t, before.GapUnacked)
				req.Cursor = tc.cursor(h, before)
				h.clock.Advance(10 * time.Second)

				result, err := h.subscribe(principalA, req)
				require.NoError(t, err)

				after := h.row(first.ID)
				assert.Equal(t, before.Generation, after.Generation)
				assert.Equal(t, before.PendingMessageID, after.PendingMessageID)
				assert.Equal(t, before.PendingBody, after.PendingBody)
				assert.Equal(t, before.AttemptCount, after.AttemptCount)
				require.NotNil(t, after.NextAttemptAt)
				assert.True(t, before.NextAttemptAt.Equal(*after.NextAttemptAt))
				assert.Equal(t, before.ClaimToken, after.ClaimToken)
				assert.Equal(t, before.CursorEventID, after.CursorEventID)
				assert.Equal(t, before.Incarnation, after.Incarnation)
				assert.Equal(t, before.ContinuityGeneration, after.ContinuityGeneration)
				assert.Equal(t, after.GapUnacked, result.Truncated, "truncated follows gap_unacked")
				assert.Equal(t, !tc.wantClear, after.GapUnacked)
				assert.Equal(t, after.CursorEventID, mustDecode(t, h, first.ID, result.Cursor).EventID, "the response cursor is the settled watermark")
				assert.Equal(t, after.ContinuityGeneration, mustDecode(t, h, first.ID, result.Cursor).ContinuityGeneration)
				assert.Equal(t, h.clock.Now().Add(24*time.Hour), *after.ExpiresAt, "the TTL is granted again")
			})
		}
	})
}

func mustDecode(t *testing.T, h *harness, id, raw string) mcpevents.Cursor {
	t.Helper()
	c, err := mcpevents.DecodeCursor(h.key, id, raw)
	require.NoError(t, err)
	return c
}

func TestLiveRefreshWithNewSecretOnlyRotates(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		before := seedDelivery(h, first.ID)

		req.Delivery.Secret = testSecret(9)
		h.clock.Advance(5 * time.Second)
		_, err := h.subscribe(principalA, req)
		require.NoError(t, err)

		after := h.row(first.ID)
		assert.Equal(t, before.Generation, after.Generation)
		assert.Equal(t, before.ClaimToken, after.ClaimToken)
		assert.Equal(t, before.PendingMessageID, after.PendingMessageID)
		assert.Equal(t, before.AttemptCount, after.AttemptCount)
		assert.NotEqual(t, before.SecretEnc, after.SecretEnc)
		current, err := mcpevents.Open(h.key, first.ID, mcpevents.SecretRoleCurrent, after.SecretEnc)
		require.NoError(t, err)
		assert.Equal(t, testSecret(9), string(current))
		previous, err := mcpevents.Open(h.key, first.ID, mcpevents.SecretRolePrevious, after.PreviousSecretEnc)
		require.NoError(t, err)
		assert.Equal(t, testSecret(1), string(previous), "the old key stays valid for dual signing")
		require.NotNil(t, after.PreviousSecretUntil)
		assert.Equal(t, h.clock.Now().Add(60*time.Second), *after.PreviousSecretUntil)
		assert.EqualValues(t, 1, h.recv.challenges.Load(), "rotation keeps consent and sends no challenge")
	})
}

func TestLiveRefreshClearsFailureAging(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		row := seedDelivery(h, first.ID)
		now := h.clock.Now()
		ok, err := h.store.UpdateMCPEventDelivery(t.Context(), db.MCPEventDeliveryUpdate{
			ID: first.ID, Incarnation: row.Incarnation, Generation: row.Generation, ClaimToken: "claim-token", Now: now, RecordFailure: true,
		})
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, 1, h.row(first.ID).FailedAttempts)

		h.mustSubscribe(req)
		after := h.row(first.ID)
		assert.Zero(t, after.FailedAttempts)
		assert.Nil(t, after.FailedSince)
	})
}

func TestNonLiveReactivation(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		setup := func(t *testing.T) (*harness, mcpevents.SubscribeRequest, db.MCPEventSubscription) {
			h := newHarness(t, open(t))
			req := h.request("example-user/" + time.Now().Format("150405.000000000"))
			first := h.mustSubscribe(req)
			seedDelivery(h, first.ID)
			row := h.row(first.ID)
			body, msg, err := mcpevents.TerminatedBody("endpoint_dead")
			require.NoError(t, err)
			ended, err := h.store.EndMCPEventSubscription(t.Context(), db.MCPEventEnd{
				ID: first.ID, Incarnation: row.Incarnation, Reason: "endpoint_dead", Now: h.clock.Now(),
				Terminated: &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: msg, Body: body},
			})
			require.NoError(t, err)
			require.Equal(t, db.MCPEventPendingTerminated, ended.PendingKind)
			return h, req, ended
		}
		t.Run("fences the generation and clears the pending control", func(t *testing.T) {
			h, req, ended := setup(t)
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, db.MCPEventStateActive, row.State)
			assert.Equal(t, ended.Generation+1, row.Generation)
			assert.Equal(t, ended.Incarnation, row.Incarnation)
			assert.Empty(t, row.PendingKind)
			assert.Empty(t, row.PendingMessageID)
			assert.Nil(t, row.PendingBody)
			assert.Zero(t, row.AttemptCount)
			assert.Nil(t, row.NextAttemptAt, "a future next_attempt_at with nothing pending hides the row from ListDue")
			assert.Empty(t, row.ClaimToken)
			assert.Empty(t, row.EndReason)
			assert.Nil(t, row.EndedAt)
			assert.Equal(t, int64(100), row.CursorEventID, "no cursor starts at head")
			assert.True(t, row.GapUnacked, "the earlier gap was never acknowledged")
			assert.True(t, result.Truncated)
		})
		t.Run("an authenticated cursor resumes", func(t *testing.T) {
			h, req, ended := setup(t)
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 90)
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, int64(90), row.CursorEventID)
			assert.False(t, result.Truncated, "a current-generation cursor proves the client learned of the gap")
			assert.Equal(t, int64(90), mustDecode(t, h, ended.ID, result.Cursor).EventID)
		})
		t.Run("an old incarnation cursor starts at head", func(t *testing.T) {
			h, req, ended := setup(t)
			req.Cursor = cursorString(h, ended, otherIncarnation, 0, 50)
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, int64(100), row.CursorEventID)
			assert.True(t, result.Truncated)
			assert.Equal(t, ended.ContinuityGeneration+1, row.ContinuityGeneration)
			assert.Equal(t, row.ContinuityGeneration, mustDecode(t, h, ended.ID, result.Cursor).ContinuityGeneration)
		})
		t.Run("an older continuity cursor starts at head", func(t *testing.T) {
			h, req, ended := setup(t)
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration-1, 90)
			result := h.mustSubscribe(req)
			assert.Equal(t, int64(100), h.row(ended.ID).CursorEventID)
			assert.True(t, result.Truncated)
		})
		t.Run("a cursor below a reset starts at head", func(t *testing.T) {
			h, req, ended := setup(t)
			h.fake.resetAbove = 95
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 90)
			result := h.mustSubscribe(req)
			assert.Equal(t, int64(100), h.row(ended.ID).CursorEventID)
			assert.True(t, result.Truncated)
		})
		t.Run("a cursor ahead of head starts at head", func(t *testing.T) {
			h, req, ended := setup(t)
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 500)
			result := h.mustSubscribe(req)
			assert.Equal(t, int64(100), h.row(ended.ID).CursorEventID)
			assert.True(t, result.Truncated)
		})
		t.Run("a bad MAC is rejected", func(t *testing.T) {
			h, req, ended := setup(t)
			good := *cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 90)
			// Replace the MAC tail with a different valid one.
			tail := "AA"
			if strings.HasSuffix(good, tail) {
				tail = "BA"
			}
			bad := good[:len(good)-2] + tail
			req.Cursor = &bad
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeInvalidParams, "invalid_cursor")
			assert.Equal(t, db.MCPEventStateEnded, h.row(ended.ID).State)
		})
		t.Run("a cursor for another subscription is rejected", func(t *testing.T) {
			h, req, ended := setup(t)
			other := mcpevents.EncodeCursor(h.key, "sub_other", mcpevents.Cursor{Incarnation: ended.Incarnation, EventID: 90})
			req.Cursor = &other
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeInvalidParams, "invalid_cursor")
		})
	})
}

func TestNewSubscriptionWithUnknownCursorStartsAtHead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		id := mcpevents.SubscriptionID(principalA.Key(), mcpevents.EventName, mcpevents.CanonicalArguments(req.Arguments), req.Delivery.URL)
		c := mcpevents.EncodeCursor(h.key, id, mcpevents.Cursor{Incarnation: otherIncarnation, EventID: 40})
		req.Cursor = &c
		result := h.mustSubscribe(req)
		assert.True(t, result.Truncated)
		assert.Equal(t, int64(100), h.row(id).CursorEventID)
		assert.Equal(t, int64(1), h.row(id).ContinuityGeneration)
	})
}

func TestNewSubscriptionShape(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		result := h.mustSubscribe(h.request("example-user"))
		row := h.row(result.ID)
		assert.Regexp(t, `^[0-9a-f]{32}$`, row.Incarnation)
		assert.False(t, result.Truncated)
		assert.True(t, result.DeliveryStatus.Active)
		assert.Equal(t, int64(100), row.CursorEventID)
		assert.NotNil(t, row.VerifiedAt)
		assert.Equal(t, "db_token", row.PrincipalAuthority)
		assert.Equal(t, "db_token:7", row.PrincipalKey)
		assert.Equal(t, "example-user", row.Actor)
		assert.NotContains(t, string(row.SecretEnc), "whsec_", "the secret is stored sealed")
		cursor := mustDecode(t, h, result.ID, result.Cursor)
		assert.Equal(t, mcpevents.Cursor{Incarnation: row.Incarnation, EventID: 100}, cursor)
	})
}

func TestRefreshByARenamedActorUpdatesTheStoredActor(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		before := h.row(first.ID)
		require.Equal(t, "example-user", before.Actor)

		renamed := principalA
		renamed.Actor = "renamed-user"
		req.Cursor = &first.Cursor
		again, err := h.subscribe(renamed, req)
		require.NoError(t, err)
		assert.Equal(t, first.ID, again.ID)
		row := h.row(first.ID)
		assert.Equal(t, "renamed-user", row.Actor)
		assert.Equal(t, before.Incarnation, row.Incarnation, "the live refresh path ran")
		assert.Equal(t, before.Generation, row.Generation)
	})
}

func TestMaxAgeReplay(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		const recipient = "example-user"
		maxAge := int64((10 * time.Minute).Milliseconds())
		setup := func(t *testing.T, events ...mcpevents.Occurrence) (*harness, mcpevents.SubscribeRequest, db.MCPEventSubscription) {
			h := newHarness(t, open(t))
			req := h.request(recipient)
			first := h.mustSubscribe(req)
			require.NoError(t, h.unsubscribe(req))
			ended := h.row(first.ID)
			h.fake.events = events
			req.MaxAgeMS = &maxAge
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 90)
			return h, req, ended
		}
		now := newTestClock().Now()
		t.Run("persists the cutoff and head", func(t *testing.T) {
			h, req, ended := setup(t, notifyOccurrence(95, testProjectA, recipient, now.Add(-time.Minute)))
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			require.NotNil(t, row.ReplayCutoff)
			assert.Equal(t, now.Add(-10*time.Minute), *row.ReplayCutoff)
			require.NotNil(t, row.ReplayHead)
			assert.Equal(t, int64(100), *row.ReplayHead)
			assert.Equal(t, int64(90), row.CursorEventID)
			assert.False(t, result.Truncated)
			assert.Equal(t, ended.ContinuityGeneration, row.ContinuityGeneration)
		})
		t.Run("the first skip is an in-place gap", func(t *testing.T) {
			h, req, ended := setup(t,
				notifyOccurrence(95, testProjectA, recipient, now.Add(-2*time.Hour)),
				notifyOccurrence(99, testProjectA, recipient, now.Add(-time.Minute)))
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, ended.ContinuityGeneration+1, row.ContinuityGeneration)
			assert.True(t, row.GapUnacked)
			assert.True(t, result.Truncated)
			assert.Equal(t, int64(90), row.CursorEventID, "the cursor stays so eligible replay continues")
			assert.Equal(t, int64(90), mustDecode(t, h, ended.ID, result.Cursor).EventID)
			assert.Equal(t, row.ContinuityGeneration, mustDecode(t, h, ended.ID, result.Cursor).ContinuityGeneration)
			assert.Empty(t, row.PendingKind, "the response carries the signal; no control is queued")
			assert.NotNil(t, row.ReplayCutoff)
			assert.NotNil(t, row.ReplayHead)
		})
		t.Run("occurrences for other recipients are not skips", func(t *testing.T) {
			h, req, ended := setup(t, notifyOccurrence(95, testProjectA, "someone-else", now.Add(-2*time.Hour)))
			result := h.mustSubscribe(req)
			assert.False(t, result.Truncated)
			assert.Equal(t, ended.ContinuityGeneration, h.row(ended.ID).ContinuityGeneration)
		})
		t.Run("a replay range over 10000 rows resets to head", func(t *testing.T) {
			events := make([]mcpevents.Occurrence, 0, 10001)
			for id := int64(1); id <= 10001; id++ {
				events = append(events, notifyOccurrence(id, testProjectA, recipient, now.Add(-time.Minute)))
			}
			h, req, ended := setup(t, events...)
			h.fake.head = 10001
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 0)
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, int64(10001), row.CursorEventID)
			assert.True(t, result.Truncated)
			assert.Equal(t, ended.ContinuityGeneration+1, row.ContinuityGeneration)
			assert.Nil(t, row.ReplayCutoff)
			assert.Nil(t, row.ReplayHead)
		})
		t.Run("a replay range over 10000 rows counts rows, not matches", func(t *testing.T) {
			events := make([]mcpevents.Occurrence, 0, 10001)
			for id := int64(1); id <= 10001; id++ {
				events = append(events, notifyOccurrence(id, testProjectA, "someone-else", now.Add(-time.Minute)))
			}
			h, req, ended := setup(t, events...)
			h.fake.head = 10001
			req.Cursor = cursorString(h, ended, ended.Incarnation, ended.ContinuityGeneration, 0)
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Equal(t, int64(10001), row.CursorEventID)
			assert.True(t, result.Truncated)
			assert.Nil(t, row.ReplayCutoff)
		})
		t.Run("a negative maxAgeMs is invalid", func(t *testing.T) {
			h, req, _ := setup(t)
			negative := int64(-1)
			req.MaxAgeMS = &negative
			_, err := h.subscribe(principalA, req)
			_ = requireWire(t, err, mcpevents.CodeInvalidParams, "invalid_max_age")
		})
		t.Run("a huge maxAgeMs is clamped without overflow", func(t *testing.T) {
			h, req, ended := setup(t, notifyOccurrence(95, testProjectA, recipient, now.Add(-2*time.Hour)))
			huge := int64(math.MaxInt64)
			req.MaxAgeMS = &huge
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.False(t, result.Truncated, "nothing is older than a hundred years")
			require.NotNil(t, row.ReplayCutoff)
			assert.True(t, row.ReplayCutoff.Before(now.Add(-99*365*24*time.Hour)))
			assert.True(t, row.ReplayCutoff.Before(now))
		})
		t.Run("is ignored with no cursor", func(t *testing.T) {
			h, req, ended := setup(t, notifyOccurrence(95, testProjectA, recipient, now.Add(-2*time.Hour)))
			req.Cursor = nil
			result := h.mustSubscribe(req)
			row := h.row(ended.ID)
			assert.Nil(t, row.ReplayCutoff)
			assert.Nil(t, row.ReplayHead)
			assert.Equal(t, int64(100), row.CursorEventID)
			assert.False(t, result.Truncated && !row.GapUnacked)
		})
	})
}

func TestLiveRowWithACursorFromTheFutureRecoversAtHead(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		before := h.row(first.ID)
		req.Cursor = cursorString(h, before, before.Incarnation, before.ContinuityGeneration, 500)

		result := h.mustSubscribe(req)
		after := h.row(first.ID)
		assert.Equal(t, before.Generation+1, after.Generation, "a position this row never reached is recovery, not a refresh")
		assert.Equal(t, int64(100), after.CursorEventID)
		assert.True(t, result.Truncated)
	})
}

func TestDynamicSelectionIsNotCappedByTheCatalogSize(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		projects := make([]mcpevents.Project, 0, 300)
		for i := range 300 {
			projects = append(projects, mcpevents.Project{UID: fmt.Sprintf("P%025d", i), ID: int64(i + 1)})
		}
		h.fake.setProjects(projects...)
		h.mustSubscribe(h.request("example-user"))
	})
}

func TestRecoveryAtTheQuotaKeepsTheRowsOwnSlot(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store, mcpevents.Limits{PrincipalActive: 1})
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		row := h.row(first.ID)
		req.Cursor = cursorString(h, row, row.Incarnation, row.ContinuityGeneration, 500)
		h.mustSubscribe(req)
		_, err := h.subscribe(principalA, h.request("example-user/second"))
		_ = requireWire(t, err, mcpevents.CodeResourceExhausted, "subscriptions")
	})
}

func TestRestoredSubscriptionRejectsFutureContinuity(t *testing.T) {
	forEachBackend(t, func(t *testing.T, store db.Storage) {
		h := newHarness(t, store)
		req := h.request("example-user")
		first := h.mustSubscribe(req)
		// The client learned of a gap after the backup represented by the row.
		newer := seedDelivery(h, first.ID)
		req.Cursor = cursorString(h, newer, newer.Incarnation, newer.ContinuityGeneration, 90)
		require.NoError(t, h.unsubscribe(h.request("example-user")))
		restored := h.row(first.ID)
		restored.ContinuityGeneration = 0
		restored.GapUnacked = false
		require.NoError(t, store.ImportMCPEventSubscription(t.Context(), restored))
		_, err := h.subscribe(principalA, req)
		_ = requireWire(t, err, mcpevents.CodeInvalidParams, "invalid_cursor")
		assert.Equal(t, restored, h.row(first.ID), "an invalid cursor leaves the restored row unchanged")
	})
}
