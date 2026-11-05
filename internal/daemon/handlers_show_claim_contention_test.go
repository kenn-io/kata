package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
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
	daemon.SetShowClaimStatusRefreshTimeoutForTest(t, 100*time.Millisecond) //nolint:kennlint // deadlinetest: controlled pending-attempt persistence waits for optional cancellation
	hegel.Test(t, func(ht *hegel.T) {
		count := hegel.Draw(ht, hegel.Integers(0, 6))
		prefix := hegel.Draw(ht, hegel.Integers(0, count))
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
		require.Len(ht, body.PendingClaims, count)
		for i, pending := range body.PendingClaims {
			require.Equal(ht, ids[count-1-i], pending.RequestUID)
			if i < prefix {
				require.NotNil(ht, pending.LastError)
				require.Contains(ht, *pending.LastError, "status refresh")
			} else {
				require.Nil(ht, pending.LastError)
			}
		}
		marker, err := spoke.DB.ClaimStatusRefreshError(t.Context(), project.ID, issue.UID)
		require.NoError(ht, err)
		require.Equal(ht, http.StatusServiceUnavailable, marker.StatusCode)
	}, hegel.WithTestCases(24), hegel.WithDatabase(t.TempDir()))
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

func TestShowIssueClaimSpokeContentionPreservesCachedHolderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		spoke, project, issue, _ := newShowClaimTimeoutSpoke(t)
		code := hegel.Draw(ht, hegel.Integers(400, 599))
		if hegel.Draw(ht, hegel.Booleans()) {
			code = http.StatusOK
		}
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if code != http.StatusOK {
				http.Error(w, "refresh unavailable", code)
				return
			}
			_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{HubNow: time.Now().UTC()})
		}))
		t.Cleanup(hub.Close)
		writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")
		holder := "cached-" + strconv.Itoa(hegel.Draw(ht, hegel.Integers(math.MinInt, math.MaxInt)))
		now := time.Now().UTC()
		claim := showIssueCachedClaim(issue, holder, now)
		require.NoError(ht, spoke.DB.ApplyClaimStatus(t.Context(), project.ID, issue.UID, db.ClaimStatus{
			Held: true, Claim: claim, HubNow: now,
		}))
		conn, err := spoke.DB.Conn(t.Context())
		require.NoError(ht, err)
		defer func() {
			_, rollbackErr := conn.ExecContext(t.Context(), "ROLLBACK")
			require.NoError(ht, rollbackErr)
			require.NoError(ht, conn.Close())
		}()
		_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
		require.NoError(ht, err)

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, spoke.URL+issuePathRef(project.ID, issue.ShortID, ""), nil)
		require.NoError(ht, err)
		resp, err := spoke.HTTP.Do(req) //nolint:gosec // synthetic loopback daemon
		require.NoError(ht, err)
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(ht, err)
		require.Equal(ht, http.StatusOK, resp.StatusCode, string(raw))
		var body showIssueClaimBody
		require.NoError(ht, json.Unmarshal(raw, &body))
		require.NotNil(ht, body.Claim)
		require.Equal(ht, holder, body.Claim.Holder)
	}, hegel.WithTestCases(24), hegel.WithDatabase(t.TempDir()))
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
