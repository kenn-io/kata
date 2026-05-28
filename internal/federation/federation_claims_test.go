package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationClientAcquireClaimPostsRequestAndParsesTimedHubFields(t *testing.T) {
	expiresAt := time.Date(2026, 5, 23, 15, 4, 5, 0, time.UTC)
	hubNow := time.Date(2026, 5, 23, 15, 0, 0, 0, time.UTC)
	var gotAuth string
	var gotBody api.ClaimActionBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/projects/42/issues/abc4/lease/actions/acquire", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{
			Granted: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Holder:            "agent-a",
				ClientKind:        "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CB",
				ProjectID:         42,
				IssueUID:          "01HZNQ7VFPK1XGD8R5MABCD4CC",
				Holder:            "agent-a",
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				ClientKind:        "cli",
				ClaimKind:         "timed",
				AcquiredAt:        hubNow,
				ExpiresAt:         &expiresAt,
				Revision:          1,
				UpdatedAt:         hubNow,
			},
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "claim-token", clientpkg.Opts{})
	require.NoError(t, err)
	got, err := client.AcquireClaim(context.Background(), 42, "abc4", ClaimRequest{
		Holder:     "agent-a",
		ClientKind: "cli",
		ClaimKind:  "timed",
		TTLSeconds: 300,
		Purpose:    "edit",
	})
	require.NoError(t, err)

	assert.Equal(t, "Bearer claim-token", gotAuth)
	assert.Equal(t, api.ClaimActionBody{
		Holder:     "agent-a",
		ClientKind: "cli",
		ClaimKind:  "timed",
		TTLSeconds: 300,
		Purpose:    "edit",
	}, gotBody)
	require.NotNil(t, got.Claim)
	assert.True(t, got.Granted)
	assert.Equal(t, expiresAt, *got.Claim.ExpiresAt)
	assert.Equal(t, hubNow, got.Claim.AcquiredAt)
}

func TestFederationClientClaimActionsUseExpectedPaths(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "claim-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.RenewClaim(context.Background(), 42, "abc4", ClaimRequest{Holder: "agent-a", TTLSeconds: 300})
	require.NoError(t, err)
	_, err = client.ReleaseClaim(context.Background(), 42, "abc4", ClaimRequest{Holder: "agent-a", Reason: "done"})
	require.NoError(t, err)

	assert.Equal(t, []string{
		"/api/v1/projects/42/issues/abc4/lease/actions/renew",
		"/api/v1/projects/42/issues/abc4/lease/actions/release",
	}, paths)
}

func TestFederationClientAcquireClaimParsesDeniedHolderResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/projects/42/issues/abc4/lease/actions/acquire", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{
			Granted: false,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Holder:            "current-holder",
				ClientKind:        "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CB",
				ProjectID:         42,
				IssueUID:          "01HZNQ7VFPK1XGD8R5MABCD4CC",
				Holder:            "current-holder",
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				ClientKind:        "cli",
				ClaimKind:         "hard",
				AcquiredAt:        time.Date(2026, 5, 23, 15, 0, 0, 0, time.UTC),
				Revision:          1,
				UpdatedAt:         time.Date(2026, 5, 23, 15, 0, 0, 0, time.UTC),
			},
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "claim-token", clientpkg.Opts{})
	require.NoError(t, err)
	got, err := client.AcquireClaim(context.Background(), 42, "abc4", ClaimRequest{
		Holder:     "requester",
		ClientKind: "cli",
		ClaimKind:  "hard",
	})
	require.NoError(t, err)

	assert.False(t, got.Granted)
	assert.Equal(t, "current-holder", got.Holder.Holder)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EA", got.Holder.HolderInstanceUID)
	require.NotNil(t, got.Claim)
	assert.Equal(t, "current-holder", got.Claim.Holder)
}

func TestFederationClientAcquireClaimAgainstRealHubParsesDeniedHolderResponse(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	hubProject := createFederatedHubForPush(t, hub)
	issue, _, err := hub.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hubProject.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)
	client, err := NewClient(ctx, hub.URL, "", clientpkg.Opts{})
	require.NoError(t, err)
	first, err := client.AcquireClaim(ctx, hubProject.ID, issue.ShortID, ClaimRequest{
		Holder:     "agent-a",
		ClientKind: "cli",
		ClaimKind:  "hard",
	})
	require.NoError(t, err)
	require.True(t, first.Granted)

	denied, err := client.AcquireClaim(ctx, hubProject.ID, issue.ShortID, ClaimRequest{
		Holder:     "agent-b",
		ClientKind: "cli",
		ClaimKind:  "hard",
	})
	require.NoError(t, err)

	assert.False(t, denied.Granted)
	assert.Equal(t, "agent-a", denied.Holder.Holder)
	require.NotNil(t, denied.Claim)
	assert.Equal(t, first.Claim.ClaimUID, denied.Claim.ClaimUID)
	assert.Equal(t, "agent-a", denied.Claim.Holder)
}

func TestFederationClientClaimStatusParsesDeniedHolderAndHubNow(t *testing.T) {
	hubNow := time.Date(2026, 5, 23, 15, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/projects/42/issues/abc4/lease", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Holder:            "agent-b",
				ClientKind:        "cli",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CB",
				ProjectID:         42,
				IssueUID:          "01HZNQ7VFPK1XGD8R5MABCD4CC",
				Holder:            "agent-b",
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				ClientKind:        "cli",
				ClaimKind:         "hard",
				AcquiredAt:        hubNow,
				Revision:          1,
				UpdatedAt:         hubNow,
			},
			HubNow: hubNow,
		}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(context.Background(), srv.URL, "claim-token", clientpkg.Opts{})
	require.NoError(t, err)
	got, err := client.ClaimStatus(context.Background(), 42, "abc4")
	require.NoError(t, err)

	assert.True(t, got.Held)
	assert.Equal(t, "agent-b", got.Holder.Holder)
	assert.Equal(t, hubNow, got.HubNow)
	require.NotNil(t, got.Claim)
	assert.Equal(t, "agent-b", got.Claim.Holder)
}
