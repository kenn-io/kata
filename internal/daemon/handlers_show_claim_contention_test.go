package daemon_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/testenv"
	"pgregory.net/rapid"
)

// Showing a spoke issue must retain its cached lease when a local writer
// prevents the optional hub refresh from updating the cache.
func TestShowIssueClaimSpokeReturnsWhileSQLiteWriterIsHeld(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusForbidden, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			spoke, project, issue, pending := newShowClaimTimeoutSpoke(t)
			holder := "cached-holder"
			if status == http.StatusBadRequest {
				// Shrunk Hegel counterexample: rejected refresh, cached holder 0.
				holder = "cached-0"
				now := time.Now().UTC()
				require.NoError(t, spoke.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{
					Held: true, Claim: showIssueCachedClaim(issue, holder, now), HubNow: now,
				}))
			}
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if status != http.StatusOK {
					http.Error(w, "refresh unavailable", status)
					return
				}
				// A successful refresh would clear the cached lease if applied.
				_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{HubNow: time.Now().UTC()})
			}))
			t.Cleanup(hub.Close)
			writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")

			conn, err := spoke.DB.Conn(t.Context())
			require.NoError(t, err)
			release := func() {
				_, rollbackErr := conn.ExecContext(t.Context(), "ROLLBACK")
				assert.NoError(t, rollbackErr)
				assert.NoError(t, conn.Close())
			}
			defer func() {
				if release != nil {
					release()
				}
			}()
			_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
			require.NoError(t, err)

			body := getShowIssueClaimBodyWithTimeout(t, spoke, project.ID, issue.ShortID, time.Second)
			require.NotNil(t, body.Claim)
			assert.Equal(t, holder, body.Claim.Holder)
			require.Len(t, body.PendingClaims, 1)
			assert.Equal(t, pending.RequestUID, body.PendingClaims[0].RequestUID)
			release()
			release = nil
			// A canceled queued refresh must not resume after writer release.
			cached, err := spoke.DB.ClaimStatusReadOnly(t.Context(), project.ID, issue.UID, time.Now().UTC())
			require.NoError(t, err)
			require.NotNil(t, cached.Claim)
			assert.Equal(t, holder, cached.Claim.Holder)
			_, err = spoke.DB.ClaimStatusRefreshError(t.Context(), project.ID, issue.UID)
			assert.ErrorIs(t, err, db.ErrNotFound)
		})
	}
}

type showClaimClearWaitStore struct{ db.Storage }

func (showClaimClearWaitStore) ClearClaimStatusRefreshError(ctx context.Context, _ int64, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

type showClaimPendingWaitStore struct {
	db.Storage
	remaining int
}

func (s *showClaimPendingWaitStore) MarkPendingClaimAttempt(ctx context.Context, uid, message string, at time.Time) error {
	if s.remaining == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	s.remaining--
	return s.Storage.MarkPendingClaimAttempt(ctx, uid, message, at)
}

func TestShowIssueClaimFailureRecordingPreservesCommittedPrefixProperty(t *testing.T) {
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "24"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	daemon.SetShowClaimStatusRefreshTimeoutForTest(t, 100*time.Millisecond) //nolint:kennlint // deadlinetest: controlled pending-attempt persistence waits for optional cancellation
	rapid.Check(t, func(rt *rapid.T) {
		count := rapid.IntRange(0, 6).Draw(rt, "pending claim count")
		prefix := rapid.IntRange(0, count).Draw(rt, "committed prefix")
		spoke := testenv.New(t, func(cfg *daemon.ServerConfig) {
			cfg.DB = &showClaimPendingWaitStore{Storage: cfg.DB, remaining: prefix}
		})
		project, issue := createShowClaimSpokeProject(t, spoke)
		now := time.Now().UTC()
		ids := make([]string, count)
		for i := range ids {
			pending := enqueueShowPendingClaim(t, spoke.DB, project.ID, issue.ShortID,
				fmt.Sprintf("pending-%d", i), now.Add(time.Duration(i)*time.Second))
			ids[i] = pending.RequestUID
		}
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
		}))
		t.Cleanup(hub.Close)
		writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")
		body := getShowIssueClaimBodyWithTimeout(t, spoke, project.ID, issue.ShortID, time.Second)
		require.Len(rt, body.PendingClaims, count)
		for i, pending := range body.PendingClaims {
			require.Equal(rt, ids[count-1-i], pending.RequestUID)
			if i < prefix {
				require.NotNil(rt, pending.LastError)
				require.Contains(rt, *pending.LastError, "status refresh")
			} else {
				require.Nil(rt, pending.LastError)
			}
		}
		marker, err := spoke.DB.ClaimStatusRefreshError(t.Context(), project.ID, issue.UID)
		require.NoError(rt, err)
		require.Equal(rt, http.StatusServiceUnavailable, marker.StatusCode)
	})
}

func TestShowIssueClaimSpokeKeepsCommittedRefreshWhenClearingTimesOut(t *testing.T) {
	spoke := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.DB = showClaimClearWaitStore{Storage: cfg.DB}
	})
	project, issue := createShowClaimSpokeProject(t, spoke)
	now := time.Now().UTC()
	require.NoError(t, spoke.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{
		Held: true, Claim: showIssueCachedClaim(issue, "old-holder", now), HubNow: now,
	}))
	require.NoError(t, spoke.DB.MarkClaimStatusRefreshError(t.Context(), project.ID, issue.UID,
		http.StatusServiceUnavailable, "old failure", now.Add(-time.Hour)))
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{HubNow: now})
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")

	body := getShowIssueClaimBodyWithTimeout(t, spoke, project.ID, issue.ShortID, time.Second)
	require.Nil(t, body.Claim, "the committed authoritative release remains visible")
	marker, err := spoke.DB.ClaimStatusRefreshError(t.Context(), project.ID, issue.UID)
	require.NoError(t, err)
	assert.Equal(t, "old failure", marker.LastError)
}

// A refresh that committed the hub's lease must report the hub's clock even
// when clearing the old failure marker runs out of budget; otherwise a spoke
// clock ahead of the hub shows the active lease as expired.
func TestShowIssueClaimSpokeKeepsHubClockWhenClearingTimesOut(t *testing.T) {
	spoke := testenv.New(t, func(cfg *daemon.ServerConfig) {
		cfg.DB = showClaimClearWaitStore{Storage: cfg.DB}
	})
	project, issue := createShowClaimSpokeProject(t, spoke)
	hubNow := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	expiresAt := hubNow.Add(time.Hour)
	require.NoError(t, spoke.DB.MarkClaimStatusRefreshError(t.Context(), project.ID, issue.UID,
		http.StatusServiceUnavailable, "old failure", time.Now().UTC().Add(-time.Hour)))
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD", Holder: "hub-holder", ClientKind: "cli",
			},
			Lease: &api.IssueClaimOut{
				ClaimUID: "01HZNQ7VFPK1XGD8R5MABCD4AA", IssueUID: issue.UID, Holder: "hub-holder",
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD", ClientKind: "cli", ClaimKind: "timed",
				AcquiredAt: hubNow, ExpiresAt: &expiresAt, Revision: 1, UpdatedAt: hubNow,
			},
			HubNow: hubNow,
		})
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")

	body := getShowIssueClaimBodyWithTimeout(t, spoke, project.ID, issue.ShortID, time.Second)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "hub-holder", body.Claim.Holder)
	require.NotNil(t, body.ClaimHubNow, "the committed refresh must keep the hub's clock")
	assert.True(t, hubNow.Equal(*body.ClaimHubNow), "got %s, want %s", body.ClaimHubNow, hubNow)
}

func TestShowIssueClaimSpokeReturnsDuringRebind(t *testing.T) {
	spoke, project, issue, _ := newShowClaimTimeoutSpoke(t)
	writeShowClaimSpokeBinding(t, spoke, project, fastFailClaimHubURL(t), "claim-token", "claim")
	release, err := federationcoord.BeginRebind(t.Context(),
		federationcoord.Key(spoke.DB.InstanceUID(), project.ID), spoke.DB, project.ID)
	require.NoError(t, err)
	defer release()

	body := getShowIssueClaimBodyWithTimeout(t, spoke, project.ID, issue.ShortID, time.Second)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
}
