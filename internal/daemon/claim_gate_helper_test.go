package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

func TestClaimGateHelperBypassesProjectsWithoutEnabledFederation(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)

	err := requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")

	require.NoError(t, err)
}

func TestClaimGateHelperHubAllowsUnclaimedIssue(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)

	err := requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")
	require.NoError(t, err)
}

func TestClaimGateHelperHubDeniesOtherLiveClaimHolder(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)

	_, err := store.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            "other",
			ClientKind:        "",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	err = requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")
	assertClaimGateHelperAPIError(t, err, http.StatusConflict, "claim_denied")
}

func TestClaimGateHelperPendingClaimDoesNotBlockUnclaimedIssue(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperHub(t, store, project.ID)
	_, err := store.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            "agent",
			ClientKind:        "",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	err = requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")

	require.NoError(t, err)
}

func TestClaimGateHelperSpokeRefreshesHubStatusBeforeCheckingCache(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	claimUID := newClaimGateHelperUID(t)
	called := false
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		require.Equal(t, "/api/v1/projects/99/issues/"+issue.ShortID+"/lease", r.URL.Path)
		_ = json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: store.InstanceUID(),
				Holder:            "agent",
				ClientKind:        "",
			},
			Claim: &api.IssueClaimOut{
				ClaimUID:          claimUID,
				IssueUID:          issue.UID,
				Holder:            "agent",
				HolderInstanceUID: store.InstanceUID(),
				ClientKind:        "",
				ClaimKind:         "hard",
				AcquiredAt:        time.Now().Add(-time.Minute).UTC(),
				Revision:          1,
				UpdatedAt:         time.Now().UTC(),
			},
			HubNow: time.Now().UTC(),
		})
	}))
	t.Cleanup(hub.Close)
	enableClaimGateHelperSpoke(t, store, project, hub.URL, 99)

	err := requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")

	require.NoError(t, err)
	assert.True(t, called)
	status, err := store.ClaimStatus(ctx, project.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, status.Held)
	require.NotNil(t, status.Claim)
	assert.Equal(t, claimUID, status.Claim.ClaimUID)
}

func TestClaimGateHelperSpokeTransportFailureFallsBackToCachedClaim(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperSpoke(t, store, project, "http://127.0.0.1:1", 99)
	require.NoError(t, store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            "agent",
			ClientKind:        "",
		},
		Claim: &db.IssueClaim{
			ClaimUID:          newClaimGateHelperUID(t),
			IssueUID:          issue.UID,
			Holder:            "agent",
			HolderInstanceUID: store.InstanceUID(),
			ClientKind:        "",
			ClaimKind:         "hard",
			AcquiredAt:        time.Now().Add(-time.Minute).UTC(),
			Revision:          1,
			UpdatedAt:         time.Now().UTC(),
		},
		HubNow: time.Now().UTC(),
	}))

	err := requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")

	require.NoError(t, err)
}

func TestClaimGateHelperPullOnlySpokeRejectsEvenWithCachedClaim(t *testing.T) {
	ctx := context.Background()
	store := openClaimGateHelperDB(t)
	project, issue := createClaimGateHelperIssue(t, store)
	enableClaimGateHelperSpokeWithPush(t, store, project, "http://127.0.0.1:1", 99, false)
	require.NoError(t, store.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            "agent",
			ClientKind:        "",
		},
		Claim: &db.IssueClaim{
			ClaimUID:          newClaimGateHelperUID(t),
			IssueUID:          issue.UID,
			Holder:            "agent",
			HolderInstanceUID: store.InstanceUID(),
			ClientKind:        "",
			ClaimKind:         "hard",
			AcquiredAt:        time.Now().Add(-time.Minute).UTC(),
			Revision:          1,
			UpdatedAt:         time.Now().UTC(),
		},
		HubNow: time.Now().UTC(),
	}))

	err := requireFederatedIssueClaim(ctx, ServerConfig{DB: store}, project.ID, issue, "agent")

	assertClaimGateHelperAPIError(t, err, http.StatusConflict, "federated_read_only")
}

func openClaimGateHelperDB(t *testing.T) *db.DB {
	t.Helper()
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	store, err := db.Open(context.Background(), filepath.Join(home, "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createClaimGateHelperIssue(t *testing.T, store *db.DB) (db.Project, db.Issue) {
	t.Helper()
	project, err := store.CreateProject(context.Background(), "claim-helper")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim helper",
		Author:    "tester",
	})
	require.NoError(t, err)
	return project, issue
}

func enableClaimGateHelperHub(t *testing.T, store *db.DB, projectID int64) {
	t.Helper()
	_, err := store.EnableProjectFederation(context.Background(), projectID, "tester")
	require.NoError(t, err)
}

func enableClaimGateHelperSpoke(t *testing.T, store *db.DB, project db.Project, hubURL string, hubProjectID int64) {
	t.Helper()
	enableClaimGateHelperSpokeWithPush(t, store, project, hubURL, hubProjectID, true)
}

func enableClaimGateHelperSpokeWithPush(t *testing.T, store *db.DB, project db.Project, hubURL string, hubProjectID int64, pushEnabled bool) {
	t.Helper()
	_, err := store.UpsertFederationBinding(context.Background(), db.FederationBinding{
		ProjectID:     project.ID,
		Role:          db.FederationRoleSpoke,
		HubURL:        hubURL,
		HubProjectID:  hubProjectID,
		HubProjectUID: project.UID,
		PushEnabled:   pushEnabled,
		Enabled:       true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hubURL,
		HubProjectID: hubProjectID,
		Token:        "claim-helper-token",
		Capabilities: "claim",
	}))
}

func newClaimGateHelperUID(t *testing.T) string {
	t.Helper()
	uid, err := katauid.New()
	require.NoError(t, err)
	return uid
}

func assertClaimGateHelperAPIError(t *testing.T, err error, status int, code string) {
	t.Helper()
	require.Error(t, err)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, status, apiErr.Status)
	assert.Equal(t, code, apiErr.Code)
}
