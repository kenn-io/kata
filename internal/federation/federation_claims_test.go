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

// TestAcquireClaimResponseCarriesCanonicalLease pins that the hub sets the
// canonical lease field on every granted arbitration, which is the precondition
// for the sync runner reading resp.Lease directly instead of coalescing it with
// the deprecated claim alias.
func TestAcquireClaimResponseCarriesCanonicalLease(t *testing.T) {
	body := api.ClaimActionResponseBody{
		Granted: true,
		Lease:   &api.IssueClaimOut{Holder: "alice", ClaimKind: "hard"},
	}
	body.MirrorDeprecatedClaimFields()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	var decoded ClaimResponse
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.Lease)
	assert.Equal(t, "alice", decoded.Lease.Holder)
	assert.Equal(t, decoded.Lease, decoded.Claim)
}

func TestClaimResponseCanonicalLeasePrefersCanonicalField(t *testing.T) {
	lease := &api.IssueClaimOut{Holder: "canonical"}
	deprecated := &api.IssueClaimOut{Holder: "deprecated"}

	got := (ClaimResponse{Lease: lease, Claim: deprecated}).canonicalLease()

	assert.Same(t, lease, got)
}

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
		assert.Equal(t, "codex", r.Header.Get("X-Kata-Teammate"))
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimActionResponseBody{}))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(db.WithEventTeammate(t.Context(), "codex"), srv.URL, "claim-token", clientpkg.Opts{})
	require.NoError(t, err)
	_, err = client.RenewClaim(db.WithEventTeammate(t.Context(), "codex"), 42, "abc4", ClaimRequest{Holder: "agent-a", TTLSeconds: 300})
	require.NoError(t, err)
	_, err = client.ReleaseClaim(db.WithEventTeammate(t.Context(), "codex"), 42, "abc4", ClaimRequest{Holder: "agent-a", Reason: "done"})
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

func TestFederationClientClaimAttributionAgainstRealHub(t *testing.T) {
	hub := testenv.New(t)
	project := createFederatedHubForPush(t, hub)
	issue, _, err := hub.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "Claim target", Author: "tester"})
	require.NoError(t, err)
	enrollment, err := hub.DB.CreateFederationEnrollment(t.Context(), db.CreateFederationEnrollmentParams{Token: "claim-attribution-token", SpokeInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA", ProjectID: &project.ID, Capabilities: "claim", Actor: "tester"})
	require.NoError(t, err)
	client, err := NewClient(t.Context(), hub.URL, enrollment.Token, clientpkg.Opts{})
	require.NoError(t, err)
	request := ClaimRequest{Holder: "untrusted-holder", ClientKind: "cli", ClaimKind: "timed", TTLSeconds: 300}
	for _, action := range []struct {
		name string
		call func(context.Context, int64, string, ClaimRequest) (ClaimResponse, error)
	}{{"acquire", client.AcquireClaim}, {"renew", client.RenewClaim}, {"release", client.ReleaseClaim}} {
		t.Run(action.name, func(t *testing.T) {
			if action.name == "renew" {
				request.TTLSeconds = 600
			}
			before, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: issue.UID, Limit: 100})
			require.NoError(t, err)
			response, err := action.call(db.WithEventTeammate(t.Context(), "codex"), project.ID, issue.ShortID, request)
			require.NoError(t, err)
			require.True(t, response.Granted)
			require.Equal(t, "tester", response.Holder.Holder)
			if action.name == "renew" {
				require.Nil(t, response.Event)
				require.NotNil(t, response.Lease)
				require.Equal(t, int64(2), response.Lease.Revision)
				after, err := hub.DB.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, IssueUID: issue.UID, Limit: 100})
				require.NoError(t, err)
				require.Equal(t, before, after)
				return
			}
			require.NotNil(t, response.Event)
			require.Equal(t, "codex", response.Event.Teammate)
			stored, err := hub.DB.EventsByUIDs(t.Context(), project.ID, []string{response.Event.UID})
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, "codex", stored[0].Teammate)
			require.Equal(t, stored[0].ContentHash, response.Event.ContentHash)
		})
	}
}
