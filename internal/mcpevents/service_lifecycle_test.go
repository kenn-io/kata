package mcpevents_test

import (
	"fmt"
	"testing"
	"time"

	"go.kenn.io/kata/internal/mcpevents"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// seedDelivery leaves the row mid-delivery: claimed, with a pending ping, one
// attempt and a scheduled retry.
func seedDelivery(h *harness, id string) db.MCPEventSubscription {
	h.t.Helper()
	ctx := h.t.Context()
	row := h.row(id)
	now := h.clock.Now()
	ok, err := h.store.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: id, Incarnation: row.Incarnation, Generation: row.Generation, Token: "claim-token", Now: now, Until: now.Add(time.Minute)})
	require.NoError(h.t, err)
	require.True(h.t, ok)
	retryAt := now.Add(30 * time.Second)
	ok, err = h.store.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: id, Incarnation: row.Incarnation, Generation: row.Generation, ClaimToken: "claim-token", Now: now,
		Pending:          &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: "evt-1", Body: []byte(`{"eventId":"evt-1"}`)},
		IncrementAttempt: true, NextAttemptAt: &retryAt,
	})
	require.NoError(h.t, err)
	require.True(h.t, ok)
	return h.row(id)
}

// SEP-3415 lets a non-replay event ignore a client cursor: a refresh with or
// without one renews the grant and leaves delivery alone.
func TestLiveRefreshIsADeliveryNoOp(t *testing.T) {
	forEachBackendOpener(t, func(t *testing.T, open opener) {
		for _, tc := range []struct {
			name   string
			cursor *string
		}{
			{"no cursor", nil},
			{"a client cursor", new("opaque-client-cursor")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := newHarness(t, open(t))
				req := h.request("example-user")
				first := h.mustSubscribe(req)
				before := seedDelivery(h, first.ID)
				req.Cursor = tc.cursor
				maxAge := int64(60_000)
				req.MaxAgeMS = &maxAge
				h.clock.Advance(10 * time.Second)

				result, err := h.subscribe(principalA, req)
				require.NoError(t, err)

				assert.Nil(t, result.Cursor)
				assert.False(t, result.Truncated)
				after := h.row(first.ID)
				assert.Equal(t, before.Generation, after.Generation)
				assert.Equal(t, before.PendingMessageID, after.PendingMessageID)
				assert.Equal(t, before.PendingBody, after.PendingBody)
				assert.Equal(t, before.AttemptCount, after.AttemptCount)
				require.NotNil(t, after.NextAttemptAt)
				assert.True(t, before.NextAttemptAt.Equal(*after.NextAttemptAt))
				assert.Equal(t, before.ClaimToken, after.ClaimToken)
				assert.Equal(t, before.Incarnation, after.Incarnation)
				assert.Equal(t, h.clock.Now().Add(24*time.Hour), *after.ExpiresAt, "the TTL is granted again")
			})
		}
	})
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
			assert.Nil(t, result.Cursor)
			assert.False(t, result.Truncated)
		})
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
		assert.False(t, row.Dirty, "a new subscription has no change to report yet")
		assert.Nil(t, result.Cursor)
		assert.NotNil(t, row.VerifiedAt)
		assert.Equal(t, "db_token", row.PrincipalAuthority)
		assert.Equal(t, "db_token:7", row.PrincipalKey)
		assert.Equal(t, "example-user", row.Actor)
		assert.NotContains(t, string(row.SecretEnc), "whsec_", "the secret is stored sealed")
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
		again, err := h.subscribe(renamed, req)
		require.NoError(t, err)
		assert.Equal(t, first.ID, again.ID)
		row := h.row(first.ID)
		assert.Equal(t, "renamed-user", row.Actor)
		assert.Equal(t, before.Incarnation, row.Incarnation, "the live refresh path ran")
		assert.Equal(t, before.Generation, row.Generation)
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
