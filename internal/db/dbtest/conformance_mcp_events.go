package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

var mcpEventsNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func mcpEventSub(id, subject string) db.MCPEventSubscription {
	return db.MCPEventSubscription{
		ID:                 "sub_" + id,
		PrincipalAuthority: "auth.example",
		PrincipalSubject:   subject,
		PrincipalKey:       "auth.example:" + subject,
		Actor:              "Example Actor",
		EventName:          "kata.inbox.changed",
		Arguments:          []byte(`{"for":"me"}`),
		CallbackURL:        "https://receiver.example/hook/" + id,
		SecretEnc:          []byte("sealed-secret"),
		Incarnation:        "inc-" + id,
		State:              db.MCPEventStateActive,
		CreatedAt:          mcpEventsNow,
		UpdatedAt:          mcpEventsNow,
	}
}

func mcpEventEnded(id, subject string, endedAt time.Time, pending bool) db.MCPEventSubscription {
	s := mcpEventSub(id, subject)
	s.State = db.MCPEventStateEnded
	s.EndReason = "unsubscribed"
	s.EndedAt = &endedAt
	if pending {
		s.PendingKind = db.MCPEventPendingTerminated
		s.PendingMessageID = "msg_terminated_" + id
		s.PendingBody = []byte(`{"type":"terminated"}`)
	}
	return s
}

func mcpEventImport(t *testing.T, s db.Storage, rows ...db.MCPEventSubscription) {
	t.Helper()
	for _, row := range rows {
		require.NoError(t, s.ImportMCPEventSubscription(t.Context(), row), row.ID)
	}
}

func mcpEventGet(t *testing.T, s db.Storage, id string) db.MCPEventSubscription {
	t.Helper()
	got, err := s.GetMCPEventSubscription(t.Context(), id)
	require.NoError(t, err)
	return got
}

func mcpEventIDs(rows []db.MCPEventSubscription) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func checkMCPEventsActivate(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("a", "alice")
	created, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Equal(t, sub.ID, created.ID)
	assert.Equal(t, "inc-a", created.Incarnation)
	assert.Equal(t, db.MCPEventStateActive, created.State)
	assert.Equal(t, []byte(`{"for":"me"}`), created.Arguments)
	assert.True(t, mcpEventsNow.Equal(created.CreatedAt))

	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "an existing row needs Expect")

	// Each expectation differs from the row in exactly one fenced field.
	stale := &db.MCPEventExpect{Incarnation: "inc-other", Generation: created.Generation, UpdatedAt: created.UpdatedAt}
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Expect: stale, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "a stale incarnation conflicts")
	stale = &db.MCPEventExpect{Incarnation: "inc-a", Generation: created.Generation + 7, UpdatedAt: created.UpdatedAt}
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Expect: stale, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "a stale generation conflicts")

	missing := mcpEventSub("missing", "alice")
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: missing, Expect: &db.MCPEventExpect{Incarnation: "inc-missing"}, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "Expect against an absent row conflicts")

	// Reactivation of an ended row replaces it when Expect matches.
	ended := mcpEventEnded("b", "alice", mcpEventsNow, true)
	mcpEventImport(t, s, ended)
	again := mcpEventSub("b", "alice")
	again.Generation = 0
	again.ClaimToken = "stale-claim"
	replaced, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{
		Subscription: again, Expect: &db.MCPEventExpect{Incarnation: "inc-b", Generation: 0, UpdatedAt: ended.UpdatedAt}, Now: mcpEventsNow,
	})
	require.NoError(t, err)
	assert.Equal(t, db.MCPEventStateActive, replaced.State)
	assert.Equal(t, int64(1), replaced.Generation, "replacement bumps the generation")
	assert.Empty(t, replaced.ClaimToken, "replacement clears the claim")
	assert.Equal(t, db.MCPEventPendingNone, replaced.PendingKind)
	assert.Empty(t, replaced.PendingMessageID)
	assert.Empty(t, replaced.PendingBody)
	assert.Nil(t, replaced.EndedAt)

	_, err = s.GetMCPEventSubscription(ctx, "sub_absent")
	assert.ErrorIs(t, err, db.ErrNotFound)

	list, err := s.ListMCPEventSubscriptions(ctx, "auth.example:alice")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sub_a", "sub_b"}, mcpEventIDs(list))
	other, err := s.ListMCPEventSubscriptions(ctx, "auth.example:nobody")
	require.NoError(t, err)
	assert.Empty(t, other)

	invalid := mcpEventSub("bad", "alice")
	invalid.State = "paused"
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: invalid, Now: mcpEventsNow})
	assert.Error(t, err)
	return nil
}

func checkMCPEventsConsent(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	verified := mcpEventSub("v", "alice")
	at := mcpEventsNow
	verified.VerifiedAt = &at
	unverified := mcpEventSub("u", "alice")
	mcpEventImport(t, s, verified, unverified)

	ok, err := s.MCPEventConsentExists(ctx, "auth.example:alice", verified.CallbackURL)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = s.MCPEventConsentExists(ctx, "auth.example:alice", unverified.CallbackURL)
	require.NoError(t, err)
	assert.False(t, ok, "an unverified row grants no consent")
	ok, err = s.MCPEventConsentExists(ctx, "auth.example:bob", verified.CallbackURL)
	require.NoError(t, err)
	assert.False(t, ok, "consent is per principal")

	active, err := s.CountActiveMCPEventSubscriptions(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, active, "both imported rows are active")
	return nil
}

func checkMCPEventsNullableRoundTrip(t *testing.T, s db.Storage) error {
	empty := mcpEventSub("empty", "alice")
	full := mcpEventSub("full", "alice")
	full.PendingKind = db.MCPEventPendingEvent
	full.PendingMessageID = "msg_1"
	full.PendingBody = []byte(`{"type":"event"}`)
	full.ClaimToken = "tok-1"
	until := mcpEventsNow.Add(time.Minute)
	full.ClaimUntil = &until
	full.PreviousSecretEnc = []byte("older")
	full.PreviousSecretUntil = &until
	full.Dirty = true
	mcpEventImport(t, s, empty, full)

	got := mcpEventGet(t, s, empty.ID)
	assert.Equal(t, db.MCPEventPendingNone, got.PendingKind)
	assert.Empty(t, got.PendingMessageID)
	assert.Empty(t, got.ClaimToken)
	assert.Empty(t, got.PendingBody)
	assert.Empty(t, got.PreviousSecretEnc)
	assert.Nil(t, got.ClaimUntil)
	assert.False(t, got.Dirty)

	got = mcpEventGet(t, s, full.ID)
	assert.Equal(t, db.MCPEventPendingEvent, got.PendingKind)
	assert.Equal(t, "msg_1", got.PendingMessageID)
	assert.Equal(t, "tok-1", got.ClaimToken)
	assert.Equal(t, []byte(`{"type":"event"}`), got.PendingBody)
	assert.Equal(t, []byte("older"), got.PreviousSecretEnc)
	require.NotNil(t, got.ClaimUntil)
	assert.True(t, until.Equal(*got.ClaimUntil))
	assert.True(t, got.Dirty)

	// "" is stored as NULL: only the row with a real pending kind is due as an
	// ended control, and an ended row with an empty kind is not.
	endedEmpty := mcpEventEnded("ended-empty", "alice", mcpEventsNow, false)
	mcpEventImport(t, s, endedEmpty)
	due, err := s.ListDueMCPEventSubscriptions(t.Context(), mcpEventsNow.Add(time.Hour), 100)
	require.NoError(t, err)
	assert.NotContains(t, mcpEventIDs(due), endedEmpty.ID)
	return nil
}

func checkMCPEventsImport(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("i", "alice")
	mcpEventImport(t, s, sub)
	changed := sub
	changed.Actor = "Renamed"
	changed.Generation = 4
	mcpEventImport(t, s, changed)
	got := mcpEventGet(t, s, sub.ID)
	assert.Equal(t, "Renamed", got.Actor)
	assert.Equal(t, int64(4), got.Generation)

	bad := sub
	bad.PrincipalKey = "wrong"
	assert.Error(t, s.ImportMCPEventSubscription(ctx, bad))
	assert.Equal(t, "Renamed", mcpEventGet(t, s, sub.ID).Actor, "a rejected import changes nothing")
	return nil
}

func checkMCPEventsQuotaRace(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	for i := range db.MCPEventPrincipalActiveMax - 1 {
		mcpEventImport(t, s, mcpEventSub(fmt.Sprintf("q%03d", i), "alice"))
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"race-x", "race-y"} {
		wg.Go(func() {
			<-start
			_, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: mcpEventSub(id, "alice"), Now: mcpEventsNow})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)

	var successes int
	var quota db.ErrMCPEventQuota
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		require.ErrorAs(t, err, &quota)
	}
	assert.Equal(t, 1, successes, "exactly one concurrent activation wins the last slot")
	assert.Equal(t, db.MCPEventQuotaPrincipal, quota.Limit)
	assert.Equal(t, db.MCPEventPrincipalActiveMax, quota.Max)

	// Another principal is unaffected, and ending a row frees a slot.
	_, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: mcpEventSub("bob-1", "bob"), Now: mcpEventsNow})
	require.NoError(t, err)
	_, err = s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_q000", Reason: "unsubscribed", Now: mcpEventsNow})
	require.NoError(t, err)
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: mcpEventSub("after-free", "alice"), Now: mcpEventsNow})
	require.NoError(t, err, "ending a row frees a slot")
	return nil
}

func checkMCPEventsDaemonQuota(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	act := func(id, subject string) error {
		_, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{
			Subscription: mcpEventSub(id, subject), Now: mcpEventsNow, PrincipalMax: 10, DaemonMax: 3,
		})
		return err
	}
	require.NoError(t, act("d1", "p1"))
	require.NoError(t, act("d2", "p2"))
	require.NoError(t, act("d3", "p3"))
	var quota db.ErrMCPEventQuota
	require.ErrorAs(t, act("d4", "p4"), &quota)
	assert.Equal(t, db.MCPEventQuotaDaemon, quota.Limit)
	assert.Equal(t, 3, quota.Max)

	// Replacing an already active row never counts against the quota.
	_, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{
		Subscription: mcpEventSub("d1", "p1"), Expect: &db.MCPEventExpect{Incarnation: "inc-d1", UpdatedAt: mcpEventsNow},
		Now: mcpEventsNow, PrincipalMax: 10, DaemonMax: 3,
	})
	assert.NoError(t, err)
	return nil
}

func checkMCPEventsRefresh(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("r", "alice")
	sub.Dirty = true
	sub.FailedAttempts = 3
	failed := mcpEventsNow.Add(-time.Hour)
	sub.FailedSince = &failed
	mcpEventImport(t, s, sub)

	expires := mcpEventsNow.Add(24 * time.Hour)
	prevUntil := mcpEventsNow.Add(time.Minute)
	got, err := s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{
		ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow,
		ExpiresAt:     &expires,
		ReplaceSecret: true, SecretEnc: []byte("new-secret"), PreviousSecretEnc: []byte("sealed-secret"), PreviousSecretUntil: &prevUntil,
		ClearFailures: true,
	})
	require.NoError(t, err)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, expires.Equal(*got.ExpiresAt))
	assert.Equal(t, []byte("new-secret"), got.SecretEnc)
	assert.Equal(t, []byte("sealed-secret"), got.PreviousSecretEnc)
	assert.Zero(t, got.FailedAttempts)
	assert.Nil(t, got.FailedSince)
	assert.True(t, got.Dirty, "refresh never clears a change waiting to be delivered")
	assert.Equal(t, int64(0), got.Generation)

	// A refresh without a new secret keeps the secret; no TTL means no expiry.
	got, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Nil(t, got.ExpiresAt)
	assert.Equal(t, []byte("new-secret"), got.SecretEnc)

	// The actor is the snapshot at the last subscribe: a refresh by a different
	// actor replaces it, and a refresh that names none keeps it.
	got, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow, Actor: "Renamed Actor"})
	require.NoError(t, err)
	assert.Equal(t, "Renamed Actor", got.Actor)
	assert.Equal(t, "Renamed Actor", mcpEventGet(t, s, sub.ID).Actor)
	got, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Equal(t, "Renamed Actor", got.Actor)

	_, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: "inc-stale", Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict)
	_, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: "sub_absent", Incarnation: "x", Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrNotFound)

	ended := mcpEventEnded("r-ended", "alice", mcpEventsNow, false)
	mcpEventImport(t, s, ended)
	_, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: ended.ID, Incarnation: ended.Incarnation, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "refresh needs an active row")
	return nil
}

func checkMCPEventsRefreshSettleInterleave(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	for _, refreshFirst := range []bool{true, false} {
		id := fmt.Sprintf("rs-%t", refreshFirst)
		sub := mcpEventSub(id, "alice")
		sub.PendingKind = db.MCPEventPendingEvent
		sub.PendingMessageID = "msg_" + id
		sub.PendingBody = []byte(`{}`)
		sub.ClaimToken = "tok"
		until := mcpEventsNow.Add(time.Minute)
		sub.ClaimUntil = &until
		mcpEventImport(t, s, sub)

		expires := mcpEventsNow.Add(time.Hour)
		refresh := func() error {
			_, err := s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow, ExpiresAt: &expires, ReplaceSecret: true, SecretEnc: []byte("rotated")})
			return err
		}
		settle := func() (bool, error) {
			return s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
				ID: sub.ID, Incarnation: sub.Incarnation, Generation: 0, ClaimToken: "tok", PendingMessageID: sub.PendingMessageID, Now: mcpEventsNow,
				ClearPending: true, ReleaseClaim: true,
			})
		}
		if refreshFirst {
			require.NoError(t, refresh())
			ok, err := settle()
			require.NoError(t, err)
			require.True(t, ok)
		} else {
			ok, err := settle()
			require.NoError(t, err)
			require.True(t, ok)
			require.NoError(t, refresh())
		}
		got := mcpEventGet(t, s, sub.ID)
		assert.Equal(t, db.MCPEventPendingNone, got.PendingKind, id)
		assert.Equal(t, []byte("rotated"), got.SecretEnc, id)
		require.NotNil(t, got.ExpiresAt, id)
		assert.True(t, expires.Equal(*got.ExpiresAt), id)
	}

	// Concurrent: both effects land whichever order the database picks.
	sub := mcpEventSub("rs-conc", "alice")
	sub.PendingKind = db.MCPEventPendingEvent
	sub.PendingMessageID = "msg_conc"
	sub.PendingBody = []byte(`{}`)
	sub.ClaimToken = "tok"
	until := mcpEventsNow.Add(time.Minute)
	sub.ClaimUntil = &until
	mcpEventImport(t, s, sub)
	expires := mcpEventsNow.Add(time.Hour)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var refreshErr, settleErr error
	var settled bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, refreshErr = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow, ExpiresAt: &expires})
	}()
	go func() {
		defer wg.Done()
		<-start
		settled, settleErr = s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
			ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok", PendingMessageID: "msg_conc", Now: mcpEventsNow,
			ClearPending: true,
		})
	}()
	close(start)
	wg.Wait()
	require.NoError(t, refreshErr)
	require.NoError(t, settleErr)
	assert.True(t, settled)
	got := mcpEventGet(t, s, sub.ID)
	assert.Equal(t, db.MCPEventPendingNone, got.PendingKind)
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, expires.Equal(*got.ExpiresAt))
	return nil
}

func checkMCPEventsClaims(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("c", "alice")
	mcpEventImport(t, s, sub)
	claim := func(token string, now time.Time) bool {
		ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
			ID: sub.ID, Incarnation: sub.Incarnation, Generation: 0, Token: token, Now: now, Until: now.Add(60 * time.Second),
		})
		require.NoError(t, err)
		return ok
	}
	update := func(token string) bool {
		ok, err := s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
			ID: sub.ID, Incarnation: sub.Incarnation, Generation: 0, ClaimToken: token, Now: mcpEventsNow, IncrementAttempt: true,
		})
		require.NoError(t, err)
		return ok
	}

	require.True(t, claim("tok-a", mcpEventsNow))
	got := mcpEventGet(t, s, sub.ID)
	assert.Equal(t, "tok-a", got.ClaimToken)
	require.NotNil(t, got.ClaimUntil)
	assert.True(t, mcpEventsNow.Add(60*time.Second).Equal(*got.ClaimUntil))

	assert.False(t, claim("tok-b", mcpEventsNow.Add(30*time.Second)), "a live claim blocks a second claimant")
	assert.False(t, claim("tok-b", mcpEventsNow.Add(60*time.Second)), "the claim is live through claim_until")

	renewed, err := s.RenewMCPEventClaim(ctx, sub.ID, sub.Incarnation, 0, "tok-a", mcpEventsNow.Add(120*time.Second))
	require.NoError(t, err)
	assert.True(t, renewed)
	renewed, err = s.RenewMCPEventClaim(ctx, sub.ID, sub.Incarnation, 0, "tok-wrong", mcpEventsNow.Add(300*time.Second))
	require.NoError(t, err)
	assert.False(t, renewed, "renewal needs the current token")
	assert.False(t, claim("tok-b", mcpEventsNow.Add(100*time.Second)), "renewal extended the claim")

	takeover := mcpEventsNow.Add(121 * time.Second)
	assert.False(t, claim("tok-a", takeover), "a re-acquisition must carry a fresh token")
	require.True(t, claim("tok-b", takeover), "the claim is free after claim_until")
	assert.False(t, update("tok-a"), "the old token is fenced after takeover")
	renewed, err = s.RenewMCPEventClaim(ctx, sub.ID, sub.Incarnation, 0, "tok-a", takeover.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, renewed)
	assert.True(t, update("tok-b"))
	assert.Equal(t, 1, mcpEventGet(t, s, sub.ID).AttemptCount)

	// Two workers listed the same row before A scheduled a retry and released it.
	retryAt := takeover.Add(30 * time.Second)
	okRetry, err := s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok-b", Now: takeover,
		NextAttemptAt: &retryAt, ReleaseClaim: true,
	})
	require.NoError(t, err)
	require.True(t, okRetry)
	assert.False(t, claim("stale-worker", takeover), "a stale list cannot bypass retry backoff")
	assert.True(t, claim("due-worker", retryAt), "the retry becomes claimable when due")

	// A claim names the generation it expects.
	ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
		ID: sub.ID, Incarnation: sub.Incarnation, Generation: 9, Token: "tok-c", Now: takeover.Add(time.Hour), Until: takeover.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: sub.ID, Incarnation: sub.Incarnation, Now: mcpEventsNow, Until: mcpEventsNow})
	assert.Error(t, err, "a claim needs a token")
	return nil
}

func checkMCPEventsIncarnation(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	first := mcpEventSub("inc", "alice")
	first.ClaimToken = "tok"
	until := mcpEventsNow.Add(time.Minute)
	first.ClaimUntil = &until
	mcpEventImport(t, s, first)
	ended, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: first.ID, Reason: "unsubscribed", Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Equal(t, int64(1), ended.Generation)

	// Delete-and-recreate is modelled by replacing the row with a fresh incarnation.
	second := first
	second.Incarnation = "inc-recreated"
	second.Generation = 1
	second.ClaimToken = "tok"
	mcpEventImport(t, s, second)

	ok, err := s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: first.ID, Incarnation: first.Incarnation, Generation: 1, ClaimToken: "tok", Now: mcpEventsNow, IncrementAttempt: true,
	})
	require.NoError(t, err)
	assert.False(t, ok, "the old incarnation is fenced")
	ok, err = s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
		ID: first.ID, Incarnation: first.Incarnation, Generation: 1, Token: "other", Now: mcpEventsNow.Add(time.Hour), Until: mcpEventsNow.Add(2 * time.Hour),
	})
	require.NoError(t, err)
	assert.False(t, ok)
	renewed, err := s.RenewMCPEventClaim(ctx, first.ID, first.Incarnation, 1, "tok", mcpEventsNow.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, renewed)
	_, err = s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: first.ID, Incarnation: first.Incarnation, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict)
	ok, err = s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: first.ID, Incarnation: second.Incarnation, Generation: 1, ClaimToken: "tok", Now: mcpEventsNow, IncrementAttempt: true,
	})
	require.NoError(t, err)
	assert.True(t, ok, "the current incarnation still works")
	return nil
}

func checkMCPEventsDeliveryTransitions(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("t", "alice")
	pastExpiry := mcpEventsNow.Add(-time.Minute)
	sub.ExpiresAt = &pastExpiry
	sub.PendingKind = db.MCPEventPendingEvent
	sub.PendingMessageID = "msg_event"
	sub.PendingBody = []byte(`{"type":"event"}`)
	mcpEventImport(t, s, sub)
	require.True(t, func() bool {
		ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: sub.ID, Incarnation: sub.Incarnation, Token: "tok", Now: mcpEventsNow, Until: mcpEventsNow.Add(time.Minute)})
		require.NoError(t, err)
		return ok
	}())
	base := func() db.MCPEventDeliveryUpdate {
		return db.MCPEventDeliveryUpdate{ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok", PendingMessageID: "msg_event", Now: mcpEventsNow}
	}
	do := func(u db.MCPEventDeliveryUpdate) bool {
		ok, err := s.UpdateMCPEventDelivery(ctx, u)
		require.NoError(t, err)
		return ok
	}

	// Attempt start rechecks expiry for event deliveries.
	start := base()
	start.IncrementAttempt = true
	start.RequireDeliverable = true
	assert.False(t, do(start), "an expired row does not deliver events")
	assert.Equal(t, 0, mcpEventGet(t, s, sub.ID).AttemptCount)

	// Retry bookkeeping.
	retry := base()
	retry.RecordFailure = true
	retry.IncrementAttempt = true
	retryAt := mcpEventsNow.Add(30 * time.Second)
	retry.NextAttemptAt = &retryAt
	lastError := "http_5xx"
	retry.LastError = &lastError
	require.True(t, do(retry))
	require.True(t, do(retry))
	got := mcpEventGet(t, s, sub.ID)
	assert.Equal(t, 2, got.AttemptCount)
	assert.Equal(t, 2, got.FailedAttempts)
	require.NotNil(t, got.FailedSince)
	assert.True(t, mcpEventsNow.Equal(*got.FailedSince))
	require.NotNil(t, got.NextAttemptAt)
	assert.True(t, retryAt.Equal(*got.NextAttemptAt))
	assert.Equal(t, "http_5xx", got.LastError)

	wrong := base()
	wrong.PendingMessageID = "msg_other"
	wrong.ClearPending = true
	assert.False(t, do(wrong), "a settlement for another pending message is stale")
	assert.Equal(t, db.MCPEventPendingEvent, mcpEventGet(t, s, sub.ID).PendingKind)

	// Settle: 410 drop style (dead letter).
	settle := base()
	settle.ClearPending = true
	settle.ClearNextAttempt = true
	settle.ClearFailure = true
	settle.IncrementDeadLetter = true
	settle.ReleaseClaim = true
	delivered := mcpEventsNow.Add(time.Second)
	settle.LastDeliveryAt = &delivered
	empty := ""
	settle.LastError = &empty
	zero := 0
	settle.AttemptCount = &zero
	require.True(t, do(settle))
	got = mcpEventGet(t, s, sub.ID)
	assert.Equal(t, db.MCPEventPendingNone, got.PendingKind)
	assert.Nil(t, got.NextAttemptAt)
	assert.Nil(t, got.FailedSince)
	assert.Zero(t, got.FailedAttempts)
	assert.Zero(t, got.AttemptCount)
	assert.Equal(t, 1, got.DeadLetterCount)
	assert.Empty(t, got.ClaimToken)
	assert.Nil(t, got.ClaimUntil)
	assert.Empty(t, got.LastError)
	require.NotNil(t, got.LastDeliveryAt)
	assert.True(t, delivered.Equal(*got.LastDeliveryAt))

	// The released claim fences the old token.
	assert.False(t, do(base()), "the claim was released")

	// Stage: a new claim turns the dirty flag into a pending ping.
	require.NoError(t, s.MarkMCPEventSubscriptionsDirty(ctx, []string{sub.ID}))
	ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: sub.ID, Incarnation: sub.Incarnation, Token: "tok2", Now: mcpEventsNow, Until: mcpEventsNow.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, ok)
	stage := db.MCPEventDeliveryUpdate{
		ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok2", Now: mcpEventsNow, ClearDirty: true,
		Pending:      &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: "evt_1", Body: []byte(`{"eventId":"evt_1"}`)},
		AttemptCount: &zero,
	}
	require.True(t, do(stage))
	got = mcpEventGet(t, s, sub.ID)
	assert.False(t, got.Dirty)
	assert.Equal(t, db.MCPEventPendingEvent, got.PendingKind)
	assert.Equal(t, "evt_1", got.PendingMessageID)
	assert.Equal(t, []byte(`{"eventId":"evt_1"}`), got.PendingBody)
	assert.Equal(t, "tok2", got.ClaimToken, "the claimant keeps its claim")

	// Ping attempts require an unexpired grant.
	controlStart := db.MCPEventDeliveryUpdate{
		ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok2", PendingMessageID: "evt_1", Now: mcpEventsNow,
		IncrementAttempt: true, RequireDeliverable: true,
	}
	assert.False(t, do(controlStart), "an expired row does not deliver pings")
	assert.Zero(t, mcpEventGet(t, s, sub.ID).AttemptCount)

	// Abandon after the retry budget, then end.
	abandon := db.MCPEventDeliveryUpdate{ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok2", PendingMessageID: "evt_1", Now: mcpEventsNow, ClearPending: true, ClearNextAttempt: true, AttemptCount: &zero}
	require.True(t, do(abandon))
	assert.Equal(t, db.MCPEventPendingNone, mcpEventGet(t, s, sub.ID).PendingKind)

	endState := db.MCPEventStateEnded
	reason := "endpoint_dead"
	end := db.MCPEventDeliveryUpdate{
		ID: sub.ID, Incarnation: sub.Incarnation, ClaimToken: "tok2", Now: mcpEventsNow,
		State: &endState, EndReason: &reason, EndedAt: &delivered, BumpGeneration: true,
		Pending: &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: "msg_terminated_t", Body: []byte(`{"type":"terminated"}`)},
	}
	require.True(t, do(end))
	got = mcpEventGet(t, s, sub.ID)
	assert.Equal(t, db.MCPEventStateEnded, got.State)
	assert.Equal(t, "endpoint_dead", got.EndReason)
	assert.Equal(t, int64(1), got.Generation)
	require.NotNil(t, got.EndedAt)

	stale := db.MCPEventDeliveryUpdate{ID: sub.ID, Incarnation: sub.Incarnation, Generation: 0, ClaimToken: "tok2", Now: mcpEventsNow, ClearPending: true}
	assert.False(t, do(stale), "the old generation is fenced")

	// Termination notices remain deliverable after the grant has ended/expired.
	terminatedStart := controlStart
	terminatedStart.Generation = 1
	terminatedStart.PendingMessageID = "msg_terminated_t"
	assert.True(t, do(terminatedStart))
	assert.Equal(t, 1, mcpEventGet(t, s, sub.ID).AttemptCount)

	// Even a termination notice needs a live claim to start an attempt.
	lapsed := terminatedStart
	lapsed.Now = mcpEventsNow.Add(24 * time.Hour)
	assert.False(t, do(lapsed), "an expired claim never persists an attempt")
	assert.Equal(t, 1, mcpEventGet(t, s, sub.ID).AttemptCount)

	invalid := base()
	invalid.ClearPending = true
	invalid.Pending = &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: "m", Body: []byte("{}")}
	_, err = s.UpdateMCPEventDelivery(ctx, invalid)
	assert.Error(t, err, "a transition cannot both set and clear the pending slot")
	return nil
}

func checkMCPEventsEnd(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	pending := mcpEventSub("e1", "alice")
	pending.PendingKind = db.MCPEventPendingEvent
	pending.PendingMessageID = "msg_old"
	pending.PendingBody = []byte(`{}`)
	mcpEventImport(t, s, pending, mcpEventSub("e2", "alice"))

	control := &db.MCPEventPending{Kind: db.MCPEventPendingTerminated, MessageID: "msg_terminated_e1", Body: []byte(`{"type":"terminated"}`)}
	got, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_e1", Reason: "principal_revoked", Now: mcpEventsNow, Terminated: control})
	require.NoError(t, err)
	assert.Equal(t, db.MCPEventStateEnded, got.State)
	assert.Equal(t, "principal_revoked", got.EndReason)
	assert.Equal(t, int64(1), got.Generation)
	assert.Equal(t, db.MCPEventPendingTerminated, got.PendingKind)
	assert.Equal(t, "msg_terminated_e1", got.PendingMessageID)
	require.NotNil(t, got.EndedAt)
	assert.True(t, mcpEventsNow.Equal(*got.EndedAt))

	again, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_e1", Reason: "unsubscribed", Now: mcpEventsNow.Add(time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, "principal_revoked", again.EndReason, "ending an ended row changes nothing")
	assert.Equal(t, int64(1), again.Generation)

	plain, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_e2", Incarnation: "inc-e2", Reason: "unsubscribed", Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Equal(t, db.MCPEventPendingNone, plain.PendingKind)

	_, err = s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_absent", Reason: "unsubscribed", Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: "sub_e1", Incarnation: "inc-stale", Reason: "unsubscribed", Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict)
	return nil
}

func checkMCPEventsListDue(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	soon := mcpEventsNow.Add(time.Minute)
	later := mcpEventsNow.Add(time.Hour)
	live := mcpEventsNow.Add(30 * time.Second)

	pending := func(sub db.MCPEventSubscription) db.MCPEventSubscription {
		sub.PendingKind, sub.PendingMessageID, sub.PendingBody = db.MCPEventPendingEvent, "evt_"+sub.ID, []byte(`{}`)
		return sub
	}
	idle := mcpEventSub("idle", "alice")
	dirty := mcpEventSub("due-dirty", "alice")
	dirty.Dirty = true
	retryDue := pending(mcpEventSub("due-retry", "alice"))
	retryDue.NextAttemptAt = &soon
	retryLater := pending(mcpEventSub("later", "alice"))
	retryLater.NextAttemptAt = &later
	claimed := mcpEventSub("claimed", "alice")
	claimed.Dirty = true
	claimed.ClaimToken = "tok"
	claimed.ClaimUntil = &live
	expiredClaim := mcpEventSub("claim-expired", "alice")
	expiredClaim.Dirty = true
	expiredClaim.ClaimToken = "tok"
	expiredClaim.ClaimUntil = &live
	endedControl := mcpEventEnded("ended-control", "alice", mcpEventsNow, true)
	endedPlain := mcpEventEnded("ended-plain", "alice", mcpEventsNow, false)
	endedDirty := mcpEventEnded("ended-dirty", "alice", mcpEventsNow, false)
	endedDirty.Dirty = true
	mcpEventImport(t, s, idle, dirty, retryDue, retryLater, claimed, expiredClaim, endedControl, endedPlain, endedDirty)

	now := mcpEventsNow.Add(time.Minute)
	// Make "claimed" still live at now and "claim-expired" expired.
	claimedUntil := now.Add(time.Minute)
	claimed.ClaimUntil = &claimedUntil
	mcpEventImport(t, s, claimed)

	due, err := s.ListDueMCPEventSubscriptions(ctx, now, 100)
	require.NoError(t, err)
	ids := mcpEventIDs(due)
	assert.ElementsMatch(t, []string{"sub_due-dirty", "sub_due-retry", "sub_claim-expired", "sub_ended-control"}, ids)
	assert.NotContains(t, ids, "sub_idle", "an active row with nothing to deliver is not due")
	assert.NotContains(t, ids, "sub_ended-plain", "an ended row without a pending control is not due")
	assert.NotContains(t, ids, "sub_ended-dirty", "an ended row has no ping to send")
	assert.NotContains(t, ids, "sub_later")
	assert.NotContains(t, ids, "sub_claimed")

	limited, err := s.ListDueMCPEventSubscriptions(ctx, now, 2)
	require.NoError(t, err)
	assert.Len(t, limited, 2)
	return nil
}

func checkMCPEventsPruneAge(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	now := mcpEventsNow
	oldPlain := mcpEventEnded("old-plain", "alice", now.Add(-8*24*time.Hour), false)
	oldPending := mcpEventEnded("old-pending", "alice", now.Add(-8*24*time.Hour), true)
	recent := mcpEventEnded("recent", "alice", now.Add(-6*24*time.Hour), false)
	active := mcpEventSub("active", "alice")
	mcpEventImport(t, s, oldPlain, oldPending, recent, active)

	removed, err := s.PruneEndedMCPEventSubscriptions(ctx, now, db.MCPEventRetention{})
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	list, err := s.ListMCPEventSubscriptions(ctx, "auth.example:alice")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sub_old-pending", "sub_recent", "sub_active"}, mcpEventIDs(list))
	return nil
}

func checkMCPEventsPruneCaps(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	now := mcpEventsNow
	at := func(h int) time.Time { return now.Add(-time.Duration(h) * time.Hour) }
	// alice: five ended rows (oldest first: a1..a5), a1 holds a pending control.
	mcpEventImport(t, s,
		mcpEventEnded("a1", "alice", at(50), true),
		mcpEventEnded("a2", "alice", at(40), false),
		mcpEventEnded("a3", "alice", at(30), false),
		mcpEventEnded("a4", "alice", at(20), false),
		mcpEventEnded("a5", "alice", at(10), false),
		mcpEventEnded("b1", "bob", at(45), false),
		mcpEventEnded("b2", "bob", at(5), false),
		mcpEventSub("live", "alice"),
	)
	removed, err := s.PruneEndedMCPEventSubscriptions(ctx, now, db.MCPEventRetention{PerPrincipal: 3, Daemon: 100})
	require.NoError(t, err)
	assert.Equal(t, 2, removed, "alice drops her two oldest deletable rows; the pending one stays")
	alice, err := s.ListMCPEventSubscriptions(ctx, "auth.example:alice")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sub_a1", "sub_a4", "sub_a5", "sub_live"}, mcpEventIDs(alice))

	// Daemon cap across principals: five ended rows remain (a1, a4, a5, b1, b2).
	removed, err = s.PruneEndedMCPEventSubscriptions(ctx, now, db.MCPEventRetention{PerPrincipal: 100, Daemon: 3})
	require.NoError(t, err)
	assert.Equal(t, 2, removed, "the daemon cap deletes the oldest deletable rows overall")
	bob, err := s.ListMCPEventSubscriptions(ctx, "auth.example:bob")
	require.NoError(t, err)
	assert.Equal(t, []string{"sub_b2"}, mcpEventIDs(bob), "b1 was oldest deletable")
	alice, err = s.ListMCPEventSubscriptions(ctx, "auth.example:alice")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"sub_a1", "sub_a5", "sub_live"}, mcpEventIDs(alice))
	return nil
}

func checkMCPEventsKeyCheck(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	value, ok, err := s.MCPEventsKeyCheck(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, value)

	stored, err := s.PutMCPEventsKeyCheckIfAbsent(ctx, "check-one")
	require.NoError(t, err)
	assert.Equal(t, "check-one", stored)
	stored, err = s.PutMCPEventsKeyCheckIfAbsent(ctx, "check-two")
	require.NoError(t, err)
	assert.Equal(t, "check-one", stored, "the first writer wins")

	value, ok, err = s.MCPEventsKeyCheck(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "check-one", value)
	return nil
}

func checkMCPEventsActivateRaceWithEnd(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("ab", "alice")
	_, err := s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Now: mcpEventsNow})
	require.NoError(t, err)
	read := mcpEventGet(t, s, sub.ID)
	expect := &db.MCPEventExpect{Incarnation: read.Incarnation, Generation: read.Generation, UpdatedAt: read.UpdatedAt}

	_, err = s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: sub.ID, Reason: "unsubscribed", Now: mcpEventsNow})
	require.NoError(t, err)
	_, err = s.ActivateMCPEventSubscription(ctx, db.MCPEventActivation{Subscription: sub, Expect: expect, Now: mcpEventsNow})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict, "an End after the read fences the stale activation")
	got := mcpEventGet(t, s, sub.ID)
	assert.Equal(t, db.MCPEventStateEnded, got.State)
	assert.Equal(t, int64(1), got.Generation)
	return nil
}

func checkMCPEventsRefreshAfterExpiry(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	// An expired row is never extended.
	expires := mcpEventsNow.Add(time.Hour)
	past := mcpEventsNow.Add(-time.Minute)
	expired := mcpEventSub("rg-expired", "alice")
	expired.ExpiresAt = &past
	mcpEventImport(t, s, expired)
	_, err := s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{ID: expired.ID, Incarnation: expired.Incarnation, Now: mcpEventsNow, ExpiresAt: &expires})
	assert.ErrorIs(t, err, db.ErrMCPEventConflict)
	assert.True(t, past.Equal(*mcpEventGet(t, s, expired.ID).ExpiresAt))
	return nil
}

func checkMCPEventsListDueFairness(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	for i := range 5 {
		sub := mcpEventSub(fmt.Sprintf("a-dirty%d", i), "alice")
		sub.Dirty = true
		mcpEventImport(t, s, sub)
	}
	due, err := s.ListDueMCPEventSubscriptions(ctx, mcpEventsNow, 100)
	require.NoError(t, err)
	require.Len(t, due, 5)
	first := due[0].ID

	// Dirty rows rotate: claiming the oldest moves it behind the others.
	ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
		ID: first, Incarnation: "inc-" + first[len("sub_"):], Token: "t", Now: mcpEventsNow.Add(time.Second), Until: mcpEventsNow.Add(time.Second),
	})
	require.NoError(t, err)
	require.True(t, ok)
	due, err = s.ListDueMCPEventSubscriptions(ctx, mcpEventsNow.Add(time.Minute), 100)
	require.NoError(t, err)
	require.Len(t, due, 5)
	assert.Equal(t, first, due[4].ID, "the most recently claimed row goes last")
	return nil
}

// Dirty marks are idempotent, apply only to active rows and never touch
// updated_at, so they cannot conflict with an activation fenced on it.
func checkMCPEventsMarkDirty(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	active := mcpEventSub("md-active", "alice")
	ended := mcpEventEnded("md-ended", "alice", mcpEventsNow, false)
	mcpEventImport(t, s, active, ended)
	require.NoError(t, s.MarkMCPEventSubscriptionsDirty(ctx, []string{active.ID, ended.ID, "sub_absent"}))
	require.NoError(t, s.MarkMCPEventSubscriptionsDirty(ctx, []string{active.ID}))
	got := mcpEventGet(t, s, active.ID)
	assert.True(t, got.Dirty)
	assert.True(t, mcpEventsNow.Equal(got.UpdatedAt), "marking never moves updated_at")
	assert.False(t, mcpEventGet(t, s, ended.ID).Dirty, "an ended row has nothing to ping")
	require.NoError(t, s.MarkMCPEventSubscriptionsDirty(ctx, nil))
	return nil
}

// A subscription records the projects its owner can see. Only a change to a
// recorded fingerprint marks it dirty, and ended rows are left alone. A
// fingerprint computed before a later change to visibility is not stored.
func checkMCPEventsUpdateCatalogs(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	active := mcpEventSub("uc-active", "alice")
	ended := mcpEventEnded("uc-ended", "alice", mcpEventsNow, false)
	mcpEventImport(t, s, active, ended)
	before := mcpEventGet(t, s, active.ID)
	project, err := s.CreateProject(ctx, "uc-project")
	require.NoError(t, err)
	update := func(versions map[string]string) bool {
		t.Helper()
		head, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		stored, err := s.UpdateMCPEventCatalogs(ctx, versions, head)
		require.NoError(t, err)
		return stored
	}

	require.True(t, update(map[string]string{active.ID: "catalog-a", ended.ID: "catalog-a"}))
	got := mcpEventGet(t, s, active.ID)
	assert.Equal(t, "catalog-a", got.CatalogVersion)
	assert.False(t, got.Dirty, "a first fingerprint is only recorded")
	assert.Empty(t, mcpEventGet(t, s, ended.ID).CatalogVersion, "ended rows are left alone")

	require.True(t, update(map[string]string{active.ID: "catalog-a"}))
	assert.False(t, mcpEventGet(t, s, active.ID).Dirty, "an unchanged fingerprint changes nothing")

	require.True(t, update(map[string]string{active.ID: "catalog-b"}))
	got = mcpEventGet(t, s, active.ID)
	assert.Equal(t, "catalog-b", got.CatalogVersion)
	assert.True(t, got.Dirty, "a changed fingerprint marks the row")
	assert.True(t, before.UpdatedAt.Equal(got.UpdatedAt), "updated_at is left alone")

	require.NoError(t, s.ForgetMCPEventCatalogs(ctx, []string{active.ID, ended.ID}))
	assert.Equal(t, db.MCPEventCatalogUnknown, mcpEventGet(t, s, active.ID).CatalogVersion)
	assert.Empty(t, mcpEventGet(t, s, ended.ID).CatalogVersion, "ended rows are left alone")
	mcpEventImport(t, s, func() db.MCPEventSubscription {
		row := mcpEventGet(t, s, active.ID)
		row.Dirty = false
		return row
	}())
	require.True(t, update(map[string]string{active.ID: "catalog-b"}))
	got = mcpEventGet(t, s, active.ID)
	assert.Equal(t, "catalog-b", got.CatalogVersion)
	assert.True(t, got.Dirty, "an unknown fingerprint marks the row on its next update")

	team, _, err := s.CreateTeam(ctx, "uc-team", db.BootstrapActor)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		change func()
		fenced bool
	}{
		{"an issue change", func() {
			_, _, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "unrelated", Author: "tester"})
			require.NoError(t, err)
		}, false},
		{"a project created", func() {
			_, _, err := s.CreateProjectAndEvent(ctx, "uc-later", "tester")
			require.NoError(t, err)
		}, true},
		{"a team membership change", func() {
			_, err := s.SetTeamMembership(ctx, team.UID, "member-actor", true, db.BootstrapActor)
			require.NoError(t, err)
		}, true},
		{"a project purge", func() {
			_, err := s.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "tester"})
			require.NoError(t, err)
		}, true},
	} {
		if tc.name == "a project purge" {
			_, _, err := s.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester", Force: true})
			require.NoError(t, err)
		}
		head, err := s.MaxEventID(ctx)
		require.NoError(t, err)
		prior := mcpEventGet(t, s, active.ID).CatalogVersion
		tc.change()
		stored, err := s.UpdateMCPEventCatalogs(ctx, map[string]string{active.ID: "catalog-" + tc.name}, head)
		require.NoError(t, err)
		assert.Equal(t, !tc.fenced, stored, tc.name)
		want := "catalog-" + tc.name
		if tc.fenced {
			want = prior
		}
		assert.Equal(t, want, mcpEventGet(t, s, active.ID).CatalogVersion, tc.name)
	}
	return nil
}

// mcpEventsRecordApplied records applied pulled events as a pull's applying
// transaction does.
func mcpEventsRecordApplied(t *testing.T, s db.Storage, eventUIDs ...string) {
	t.Helper()
	beginner, ok := s.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok)
	tx, err := beginner.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, db.RecordMCPEventsRematch(t.Context(), tx, eventUIDs))
	require.NoError(t, tx.Commit())
}

// The rematch record spans the applied events across pulls. A newer pull
// replaces its token, so a worker that read an older record cannot change it;
// an acknowledgment short of the end keeps the rest; and starting the scan
// position, which reloads everyone, drops it.
func checkMCPEventsRematch(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "rematch-project")
	require.NoError(t, err)
	_, first, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "first", Author: "tester"})
	require.NoError(t, err)
	_, second, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "second", Author: "tester"})
	require.NoError(t, err)
	_, third, err := s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "third", Author: "tester"})
	require.NoError(t, err)
	_, _, _, ok, err := s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	require.False(t, ok)

	mcpEventsRecordApplied(t, s, second.UID)
	from, through, _, ok, err := s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, [2]int64{second.ID, second.ID}, [2]int64{from, through})
	mcpEventsRecordApplied(t, s, first.UID, "unknown-event-uid")
	from, through, older, _, err := s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	assert.Equal(t, [2]int64{first.ID, second.ID}, [2]int64{from, through}, "a later pull widens the range down")

	mcpEventsRecordApplied(t, s, third.UID)
	from, through, newer, _, err := s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	assert.Equal(t, [2]int64{first.ID, third.ID}, [2]int64{from, through}, "a later pull widens the range up")
	acked, err := s.AcknowledgeMCPEventsRematch(ctx, older, third.ID)
	require.NoError(t, err)
	assert.False(t, acked, "a stale token cannot acknowledge a newer record")

	acked, err = s.AcknowledgeMCPEventsRematch(ctx, newer, second.ID)
	require.NoError(t, err)
	assert.True(t, acked)
	from, through, rest, ok, err := s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, [2]int64{second.ID + 1, third.ID}, [2]int64{from, through}, "events past the acknowledgment stay")
	acked, err = s.AcknowledgeMCPEventsRematch(ctx, newer, third.ID)
	require.NoError(t, err)
	assert.False(t, acked, "an acknowledgment takes a fresh token")
	acked, err = s.AcknowledgeMCPEventsRematch(ctx, rest, third.ID)
	require.NoError(t, err)
	assert.True(t, acked)
	_, _, _, ok, err = s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	assert.False(t, ok)

	mcpEventsRecordApplied(t, s, first.UID)
	_, err = s.InitializeMCPEventsScanPosition(ctx, second.ID)
	require.NoError(t, err)
	_, _, _, ok, err = s.MCPEventsRematch(ctx)
	require.NoError(t, err)
	assert.False(t, ok, "starting the scan position reloads everyone")
	return nil
}

// The scan position starts once, marking every active row dirty, and then
// only moves forward from the value a caller read.
func checkMCPEventsScanPosition(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	active := mcpEventSub("sp-active", "alice")
	ended := mcpEventEnded("sp-ended", "alice", mcpEventsNow, false)
	mcpEventImport(t, s, active, ended)
	_, ok, err := s.MCPEventsScanPosition(ctx)
	require.NoError(t, err)
	assert.False(t, ok)

	initialized, err := s.InitializeMCPEventsScanPosition(ctx, 10)
	require.NoError(t, err)
	assert.True(t, initialized)
	assert.True(t, mcpEventGet(t, s, active.ID).Dirty, "a first scan reloads every subscriber")
	assert.False(t, mcpEventGet(t, s, ended.ID).Dirty)
	position, ok, err := s.MCPEventsScanPosition(ctx)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, int64(10), position)

	initialized, err = s.InitializeMCPEventsScanPosition(ctx, 99)
	require.NoError(t, err)
	assert.False(t, initialized, "an existing position is kept")

	advanced, err := s.AdvanceMCPEventsScanPosition(ctx, 10, 15)
	require.NoError(t, err)
	assert.True(t, advanced)
	advanced, err = s.AdvanceMCPEventsScanPosition(ctx, 10, 20)
	require.NoError(t, err)
	assert.False(t, advanced, "a stale reader cannot move the position")
	_, err = s.AdvanceMCPEventsScanPosition(ctx, 15, 14)
	assert.Error(t, err, "the position never moves backwards")
	position, _, err = s.MCPEventsScanPosition(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(15), position)
	return nil
}

// The ordinary event stream hides the system project, where access audit
// events live. The delivery worker reads the ones that change visibility.
func checkMCPEventsAccessChanges(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	project, err := s.CreateProject(ctx, "access-project")
	require.NoError(t, err)
	before, err := s.MaxEventID(ctx)
	require.NoError(t, err)
	team, _, err := s.CreateTeam(ctx, "access-team", db.BootstrapActor)
	require.NoError(t, err)
	_, err = s.SetTeamMembership(ctx, team.UID, "member-actor", true, db.BootstrapActor)
	require.NoError(t, err)
	_, policy, err := s.SetProjectAccessPolicy(ctx, db.ProjectAccessPolicy{ProjectUID: project.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, db.BootstrapActor)
	require.NoError(t, err)
	_, _, err = s.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "ordinary", Author: "tester"})
	require.NoError(t, err)
	head, err := s.MaxEventID(ctx)
	require.NoError(t, err)

	changes, err := s.MCPEventsAccessChangesAfter(ctx, before, head, 10)
	require.NoError(t, err)
	types := make([]string, 0, len(changes))
	for _, change := range changes {
		types = append(types, change.Type)
		assert.Equal(t, db.SystemProjectUID, change.ProjectUID)
	}
	assert.Equal(t, []string{"team.membership_changed", "project.access_changed"}, types, "team creation and ordinary events are not access changes")
	require.Len(t, changes, 2)
	assert.Contains(t, changes[0].Payload, `"member-actor"`)
	assert.Contains(t, changes[1].Payload, project.UID)

	through, err := s.MCPEventsAccessChangesAfter(ctx, before, policy.ID-1, 10)
	require.NoError(t, err)
	assert.Len(t, through, 1, "the upper bound is inclusive and respected")
	limited, err := s.MCPEventsAccessChangesAfter(ctx, before, head, 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)
	none, err := s.MCPEventsAccessChangesAfter(ctx, policy.ID, head, 10)
	require.NoError(t, err)
	assert.Empty(t, none)
	return nil
}

func checkMCPEventsEndReleasesClaim(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	sub := mcpEventSub("er", "alice")
	mcpEventImport(t, s, sub)
	ok, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: sub.ID, Incarnation: sub.Incarnation, Token: "tok", Now: mcpEventsNow, Until: mcpEventsNow.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, ok)
	ended, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: sub.ID, Reason: "unsubscribed", Now: mcpEventsNow})
	require.NoError(t, err)
	assert.Empty(t, ended.ClaimToken)
	assert.Nil(t, ended.ClaimUntil)
	renewed, err := s.RenewMCPEventClaim(ctx, sub.ID, sub.Incarnation, ended.Generation, "tok", mcpEventsNow.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, renewed, "a stale holder cannot renew after End")

	// A bumped generation also fences renewal.
	other := mcpEventSub("er2", "alice")
	mcpEventImport(t, s, other)
	ok, err = s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{ID: other.ID, Incarnation: other.Incarnation, Token: "tok", Now: mcpEventsNow, Until: mcpEventsNow.Add(time.Minute)})
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{ID: other.ID, Incarnation: other.Incarnation, ClaimToken: "tok", Now: mcpEventsNow, BumpGeneration: true})
	require.NoError(t, err)
	require.True(t, ok)
	renewed, err = s.RenewMCPEventClaim(ctx, other.ID, other.Incarnation, 0, "tok", mcpEventsNow.Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, renewed)
	return nil
}

func checkMCPEventsExport(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	pending := mcpEventEnded("b-pending", "bob", mcpEventsNow, true)
	pending.PreviousSecretEnc = []byte("older")
	mcpEventImport(t, s, mcpEventSub("c-live", "alice"), pending)

	var got []db.MCPEventSubscription
	for row, err := range s.ExportMCPEventSubscriptions(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		got = append(got, row.Subscription())
	}
	require.Len(t, got, 2)
	assert.Equal(t, []string{"sub_b-pending", "sub_c-live"}, mcpEventIDs(got), "ordered by id")
	assert.Equal(t, mcpEventGet(t, s, "sub_b-pending"), got[0])
	assert.Equal(t, mcpEventGet(t, s, "sub_c-live"), got[1])

	// Subscriptions belong to principals, not projects: a project export never
	// carries them.
	project := int64(1)
	for _, err := range s.ExportMCPEventSubscriptions(ctx, db.ExportFilter{ProjectID: &project}) {
		require.NoError(t, err)
		assert.Fail(t, "project-scoped export returned a subscription")
	}
	return nil
}

// End races a claimed worker's generation bump. Neither ordering may leave
// pending delivery work or resurrect the subscription after both commits.
func checkMCPEventsEndSurvivesGenerationBump(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	for i := range 20 {
		sub := mcpEventSub(fmt.Sprintf("eb%d", i), "example-user")
		mcpEventImport(t, s, sub)
		claimed, err := s.ClaimMCPEventSubscription(ctx, db.MCPEventClaim{
			ID: sub.ID, Incarnation: sub.Incarnation, Generation: sub.Generation,
			Token: "worker", Now: mcpEventsNow, Until: mcpEventsNow.Add(time.Minute),
		})
		require.NoError(t, err)
		require.True(t, claimed)

		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		type bumpResult struct {
			updated bool
			err     error
		}
		bumped := make(chan bumpResult, 1)
		ended := make(chan error, 1)
		go func() {
			ready.Done()
			<-start
			updated, err := s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
				ID: sub.ID, Incarnation: sub.Incarnation, Generation: sub.Generation, ClaimToken: "worker", Now: mcpEventsNow,
				BumpGeneration: true,
				Pending:        &db.MCPEventPending{Kind: db.MCPEventPendingEvent, MessageID: "evt", Body: []byte(`{"eventId":"evt"}`)},
			})
			bumped <- bumpResult{updated, err}
		}()
		go func() {
			ready.Done()
			<-start
			_, err := s.EndMCPEventSubscription(ctx, db.MCPEventEnd{ID: sub.ID, Incarnation: sub.Incarnation, Reason: "unsubscribed", Now: mcpEventsNow})
			ended <- err
		}()
		ready.Wait()
		close(start)
		bump := <-bumped
		endErr := <-ended
		require.NoError(t, bump.err)
		require.NoError(t, endErr)

		persisted := mcpEventGet(t, s, sub.ID)
		assert.Equal(t, db.MCPEventStateEnded, persisted.State)
		assert.Equal(t, "unsubscribed", persisted.EndReason)
		assert.Empty(t, persisted.ClaimToken)
		assert.Nil(t, persisted.ClaimUntil)
		assert.Equal(t, db.MCPEventPendingNone, persisted.PendingKind)
		wantGeneration := sub.Generation + 1
		if bump.updated {
			wantGeneration++
		}
		assert.Equal(t, wantGeneration, persisted.Generation)
	}
	return nil
}

// Expiry must supersede live claims and future retries without ending renewed,
// unlimited, or already-ended grants.
func checkMCPEventsExpiryMaintenance(t *testing.T, s db.Storage) error {
	ctx := t.Context()
	now := mcpEventsNow
	later := now.Add(time.Hour)
	expired := mcpEventSub("expiry-claimed", "example-user")
	expired.ExpiresAt = &now
	expired.ClaimToken, expired.ClaimUntil = "old-claim", &later
	expired.NextAttemptAt = &later
	expired.PendingKind = db.MCPEventPendingEvent
	expired.PendingMessageID = "msg-event"
	expired.PendingBody = []byte(`{"type":"event"}`)
	expired.Dirty = true
	renewed := mcpEventSub("expiry-renewed", "example-user")
	soon := now.Add(time.Minute)
	renewed.ExpiresAt = &soon
	unlimited := mcpEventSub("expiry-unlimited", "example-user")
	ended := mcpEventEnded("expiry-ended", "example-user", now, true)
	mcpEventImport(t, s, expired, renewed, unlimited, ended)
	_, err := s.RefreshMCPEventSubscription(ctx, db.MCPEventRefresh{
		ID: renewed.ID, Incarnation: renewed.Incarnation, Now: now, ExpiresAt: &later,
	})
	require.NoError(t, err)
	sweepAt := now.Add(2 * time.Minute)
	count, err := s.ExpireMCPEventSubscriptions(ctx, sweepAt)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	got := mcpEventGet(t, s, expired.ID)
	assert.Equal(t, db.MCPEventStateEnded, got.State)
	assert.Equal(t, "expired", got.EndReason)
	assert.Equal(t, int64(1), got.Generation)
	assert.Empty(t, got.PendingKind)
	assert.Empty(t, got.PendingBody)
	assert.Empty(t, got.PendingMessageID)
	assert.False(t, got.Dirty)
	assert.Nil(t, got.NextAttemptAt)
	assert.Empty(t, got.ClaimToken)
	assert.Nil(t, got.ClaimUntil)
	require.NotNil(t, got.EndedAt)
	assert.True(t, sweepAt.Equal(*got.EndedAt))
	won, err := s.UpdateMCPEventDelivery(ctx, db.MCPEventDeliveryUpdate{
		ID: expired.ID, Incarnation: expired.Incarnation, Generation: expired.Generation,
		ClaimToken: expired.ClaimToken, Now: sweepAt, ClearPending: true,
	})
	require.NoError(t, err)
	assert.False(t, won, "the old worker cannot settle after expiry")
	assert.Equal(t, db.MCPEventStateActive, mcpEventGet(t, s, renewed.ID).State)
	assert.Equal(t, db.MCPEventStateActive, mcpEventGet(t, s, unlimited.ID).State)
	assert.Equal(t, db.MCPEventPendingTerminated, mcpEventGet(t, s, ended.ID).PendingKind)
	count, err = s.ExpireMCPEventSubscriptions(ctx, sweepAt)
	require.NoError(t, err)
	assert.Zero(t, count, "repeated maintenance does not rewrite ended grants")
	return nil
}
