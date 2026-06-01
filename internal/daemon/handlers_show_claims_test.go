package daemon_test

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
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

type showIssueClaimBody struct {
	Claim               *api.IssueClaimOut      `json:"claim,omitempty"`
	PendingClaims       []api.PendingClaimOut   `json:"pending_claims,omitempty"`
	ClaimHubNow         *time.Time              `json:"claim_hub_now,omitempty"`
	ClaimViolations     []api.ClaimViolationOut `json:"claim_violations,omitempty"`
	ClaimViolationCount *int64                  `json:"claim_violation_count,omitempty"`
}

func TestShowIssueClaimIncludesLiveHubClaimAndHubNow(t *testing.T) {
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)

	var acquired claimResponseBody
	resp := claimPost(t, hub, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "hub-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &acquired)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, acquired.Claim.ClaimUID, body.Claim.ClaimUID)
	assert.Equal(t, "hub-cli", body.Claim.Holder)
	require.NotNil(t, body.ClaimHubNow)
	assert.False(t, body.ClaimHubNow.IsZero())
	assert.Empty(t, body.PendingClaims)
}

func TestShowIssueClaimInsecureReadonlyOmitsUnauthenticatedClaimHydration(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t, testenv.WithInsecureReadonly())
	project, issue := createClaimHubIssue(t, hub)
	_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: hub.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	offending := claimAuditDeliveryRemoteEvent(t, project, federationTestSpokeUID, issue.UID,
		"01HZNQ7VFPK1XGD8R5MABCD4VV", "issue.updated", "remote-agent", time.Now().UTC().UnixMilli())
	_, err = hub.DB.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID:        project.ID,
		SpokeInstanceUID: federationTestSpokeUID,
		Events: []db.FederationIngestEvent{{
			SourceEventID: 1,
			Event:         offending,
		}},
	})
	require.NoError(t, err)
	assertShowEventCount(t, hub.DB, "claim.violated", 1)

	resp, raw := envDoRaw(t, hub, http.MethodGet, issuePathRef(project.ID, issue.ShortID, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var rawBody map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rawBody))
	assert.NotContains(t, rawBody, "claim")
	assert.NotContains(t, rawBody, "pending_claims")
	assert.NotContains(t, rawBody, "claim_hub_now")
	assert.NotContains(t, rawBody, "claim_violations")
	assert.NotContains(t, rawBody, "claim_violation_count")
}

func TestShowIssueClaimOmitsReleasedHubClaim(t *testing.T) {
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	resp := claimPost(t, hub, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "hub-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp = claimPost(t, hub, project.ID, issue.ShortID, "release", map[string]any{
		"holder":      "hub-cli",
		"client_kind": "cli",
		"reason":      "done",
	}, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)
	assert.Nil(t, body.Claim)
	assert.Nil(t, body.ClaimHubNow)
	assert.Empty(t, body.PendingClaims)
}

func TestShowIssueClaimIncludesTimedClaimExpiresAt(t *testing.T) {
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	resp := claimPost(t, hub, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "hub-cli",
		"client_kind": "cli",
		"claim_kind":  "timed",
		"ttl_seconds": 600,
	}, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	require.NotNil(t, body.Claim.ExpiresAt)
	assert.True(t, body.Claim.ExpiresAt.After(body.Claim.AcquiredAt))
}

func TestShowIssueClaimExpiresTimedClaimBeforeViolationHydration(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	now := time.Now().UTC()
	_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Hour,
		Now:       now,
	})
	require.NoError(t, err)
	offendingSpokeUID := "01HZNQ7VFPK1XGD8R5MABCD4EX"
	offending := claimAuditDeliveryRemoteEvent(t, project, offendingSpokeUID, issue.UID,
		"01HZNQ7VFPK1XGD8R5MABCD4VW", "issue.updated", "remote-agent", now.Add(time.Minute).UnixMilli())
	_, err = hub.DB.IngestFederationEvents(ctx, db.FederationIngestParams{
		ProjectID:        project.ID,
		SpokeInstanceUID: offendingSpokeUID,
		Events: []db.FederationIngestEvent{{
			SourceEventID: 1,
			Event:         offending,
		}},
	})
	require.NoError(t, err)
	assertShowEventCount(t, hub.DB, "claim.violated", 1)
	_, err = hub.DB.ExecContext(ctx,
		`UPDATE issue_claims SET expires_at = ? WHERE issue_uid = ? AND released_at IS NULL`,
		now.Add(-time.Minute).Format("2006-01-02T15:04:05.000Z"), issue.UID)
	require.NoError(t, err)

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)

	assert.Nil(t, body.Claim)
	assert.Nil(t, body.ClaimViolationCount)
	assert.Empty(t, body.ClaimViolations)
	assertShowEventCount(t, hub.DB, "claim.expired", 1)
}

func TestShowIssueClaimHubExpiryBroadcastsCommittedEvent(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	now := time.Now().UTC()
	_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: hub.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       now.Add(-2 * time.Minute),
	})
	require.NoError(t, err)
	sub := hub.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)

	assert.Nil(t, body.Claim)
	msg := receiveMsg(t, sub.Ch, time.Second, "show should broadcast claim expiry")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "claim.expired", msg.Event.Type)
}

func TestShowIssueClaimSpokeFallbackDoesNotExpireCachedTimedClaim(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	cachedAt := time.Now().UTC().Add(-2 * time.Minute)
	expiresAt := cachedAt.Add(time.Minute)
	require.NoError(t, spoke.DB.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached-holder",
			ClientKind:        "cli",
		},
		Claim: &db.IssueClaim{
			ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4AA",
			ProjectID:         project.ID,
			IssueID:           issue.ID,
			IssueUID:          issue.UID,
			Holder:            "cached-holder",
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			ClientKind:        "cli",
			ClaimKind:         "timed",
			AcquiredAt:        cachedAt,
			ExpiresAt:         &expiresAt,
			Revision:          1,
			UpdatedAt:         cachedAt,
		},
		HubNow: cachedAt,
	}))
	writeShowClaimSpokeBinding(t, spoke, project, "http://127.0.0.1:1", "claim-token", "claim")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)

	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	assertShowLiveClaimCount(t, spoke.DB, issue.UID, 1)
	assertShowEventCount(t, spoke.DB, "claim.expired", 0)
}

func TestShowIssueClaimIncludesPendingClaimsNewestFirst(t *testing.T) {
	ctx := context.Background()
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	oldestAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	rejectedAt := oldestAt.Add(time.Minute)
	newestAt := oldestAt.Add(2 * time.Minute)
	oldest := enqueueShowPendingClaim(t, hub.DB, project.ID, issue.ShortID, "oldest-cli", oldestAt)
	rejected := enqueueShowPendingClaim(t, hub.DB, project.ID, issue.ShortID, "rejected-cli", rejectedAt)
	newest := enqueueShowPendingClaim(t, hub.DB, project.ID, issue.ShortID, "newest-cli", newestAt)
	require.NoError(t, hub.DB.RejectPendingClaim(ctx, rejected.RequestUID, "not needed", rejectedAt))

	body := getShowIssueClaimBody(t, hub, project.ID, issue.ShortID)
	assert.Nil(t, body.Claim)
	require.Len(t, body.PendingClaims, 2)
	assert.Equal(t, newest.RequestUID, body.PendingClaims[0].RequestUID)
	assert.Equal(t, oldest.RequestUID, body.PendingClaims[1].RequestUID)
	require.NotNil(t, body.ClaimHubNow)
	assert.False(t, body.ClaimHubNow.IsZero())
}

func TestShowIssueClaimPendingClaimsUseAPIShape(t *testing.T) {
	hub := testenv.New(t)
	project, issue := createClaimHubIssue(t, hub)
	pending := enqueueShowPendingClaim(t, hub.DB, project.ID, issue.ShortID, "pending-cli", time.Now().UTC())

	resp, raw := envDoRaw(t, hub, http.MethodGet, issuePathRef(project.ID, issue.ShortID, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	claims, ok := body["pending_claims"].([]any)
	require.True(t, ok, "pending_claims should be an array in body: %s", raw)
	require.Len(t, claims, 1)
	claim, ok := claims[0].(map[string]any)
	require.True(t, ok, "pending claim should be an object in body: %s", raw)
	assert.Equal(t, pending.RequestUID, claim["request_uid"])
	assert.NotContains(t, claim, "RequestUID")
	assert.NotContains(t, claim, "ID")
	assert.NotContains(t, claim, "IssueID")
	assert.NotContains(t, claim, "id")
	assert.NotContains(t, claim, "project_id")
	assert.NotContains(t, claim, "issue_id")
}

func TestShowIssueClaimNonFederatedUnclaimedIssueOmitsClaimFields(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "plain")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "plain issue",
		Author:    "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, issuePathRef(project.ID, issue.ShortID, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var rawBody map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rawBody))
	assert.NotContains(t, rawBody, "claim")
	assert.NotContains(t, rawBody, "pending_claims")
	assert.NotContains(t, rawBody, "claim_hub_now")
}

func TestShowIssueClaimNonFederatedShowDoesNotExpireFederatedClaims(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	hubProject, hubIssue := createClaimHubIssue(t, env)
	acquiredAt := time.Now().UTC().Add(-2 * time.Minute)
	_, err := env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID,
		IssueRef:  hubIssue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       acquiredAt,
	})
	require.NoError(t, err)
	assertShowLiveClaimCount(t, env.DB, hubIssue.UID, 1)
	assertShowEventCount(t, env.DB, "claim.expired", 0)

	plainProject, err := env.DB.CreateProject(ctx, "plain")
	require.NoError(t, err)
	plainIssue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: plainProject.ID,
		Title:     "plain issue",
		Author:    "tester",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, issuePathRef(plainProject.ID, plainIssue.ShortID, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var rawBody map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rawBody))
	assert.NotContains(t, rawBody, "claim")
	assert.NotContains(t, rawBody, "pending_claims")
	assert.NotContains(t, rawBody, "claim_hub_now")
	assertShowLiveClaimCount(t, env.DB, hubIssue.UID, 1)
	assertShowEventCount(t, env.DB, "claim.expired", 0)
}

func TestShowIssueClaimIncludeDeletedSkipsClaimHydration(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)
	_, err := env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "hub-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)
	_, _, changed, err := env.DB.SoftDeleteIssue(ctx, issue.ID, "tester")
	require.NoError(t, err)
	require.True(t, changed)

	resp, raw := envDoRaw(t, env, http.MethodGet, issuePathRef(project.ID, issue.ShortID, "")+"?include_deleted=true", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var rawBody map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &rawBody))
	assert.NotContains(t, rawBody, "claim")
	assert.NotContains(t, rawBody, "pending_claims")
	assert.NotContains(t, rawBody, "claim_hub_now")
}

func TestShowIssueClaimRefreshCallsHubAndReturnsFreshClaim(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	hubNow := time.Date(2026, 5, 23, 15, 0, 0, 0, time.UTC)
	var calls int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "Bearer claim-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/projects/42/issues/"+issue.ShortID+"/lease", r.URL.Path)
		require.NoError(t, json.NewEncoder(w).Encode(api.ClaimStatusBody{
			Held: true,
			Holder: api.ClaimPrincipalOut{
				HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
				Holder:            "hub-holder",
				ClientKind:        "cli",
			},
			Claim:  showIssueClaimOut(issue, "hub-holder", hubNow),
			HubNow: hubNow,
		}))
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	assert.Equal(t, 1, calls)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "hub-holder", body.Claim.Holder)
	require.NotNil(t, body.ClaimHubNow)
	assert.Equal(t, hubNow, *body.ClaimHubNow)

	cached, err := spoke.DB.ClaimStatus(ctx, project.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, cached.Held)
	assert.Equal(t, "hub-holder", cached.Holder.Holder)
}

func TestShowIssueClaimRefreshHubUnreachableFallsBackToCachedClaimAndPending(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	cachedAt := time.Date(2026, 5, 23, 13, 0, 0, 0, time.UTC)
	require.NoError(t, spoke.DB.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached-holder",
			ClientKind:        "cli",
		},
		Claim:  showIssueCachedClaim(issue, "cached-holder", cachedAt),
		HubNow: cachedAt,
	}))
	pending := enqueueShowPendingClaim(t, spoke.DB, project.ID, issue.ShortID, "pending-cli", cachedAt.Add(time.Minute))
	writeShowClaimSpokeBinding(t, spoke, project, "http://127.0.0.1:1", "claim-token", "claim")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	require.Len(t, body.PendingClaims, 1)
	assert.Equal(t, pending.RequestUID, body.PendingClaims[0].RequestUID)
}

func TestShowIssueClaimRefreshForbiddenRecordsErrorAndDoesNotHotLoop(t *testing.T) {
	testShowIssueClaimRefreshStatusErrorDoesNotHotLoop(t, http.StatusForbidden)
}

func TestShowIssueClaimRefreshUnauthorizedRecordsErrorAndDoesNotHotLoop(t *testing.T) {
	testShowIssueClaimRefreshStatusErrorDoesNotHotLoop(t, http.StatusUnauthorized)
}

func TestShowIssueClaimRefreshForbiddenWithoutPendingDoesNotHotLoop(t *testing.T) {
	testShowIssueClaimRefreshStatusErrorWithoutPendingDoesNotHotLoop(t, http.StatusForbidden)
}

func TestShowIssueClaimRefreshUnauthorizedWithoutPendingDoesNotHotLoop(t *testing.T) {
	testShowIssueClaimRefreshStatusErrorWithoutPendingDoesNotHotLoop(t, http.StatusUnauthorized)
}

func TestShowIssueClaimRefreshInternalServerErrorWithoutPendingDoesNotHotLoop(t *testing.T) {
	testShowIssueClaimRefreshStatusErrorWithoutPendingDoesNotHotLoop(t, http.StatusInternalServerError)
}

func TestShowIssueClaimRefreshTransportFailureWithoutPendingDoesNotHotLoop(t *testing.T) {
	ctx := context.Background()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	cachedAt := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	require.NoError(t, spoke.DB.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached-holder",
			ClientKind:        "cli",
		},
		Claim:  showIssueCachedClaim(issue, "cached-holder", cachedAt),
		HubNow: cachedAt,
	}))
	var calls int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		hijacker, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, err := hijacker.Hijack()
		require.NoError(t, err)
		_ = conn.Close()
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "claim")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	body = getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	assert.Equal(t, 1, calls)
}

func getShowIssueClaimBody(t *testing.T, env *testenv.Env, projectID int64, ref string) showIssueClaimBody {
	t.Helper()
	resp, raw := envDoRaw(t, env, http.MethodGet, issuePathRef(projectID, ref, ""), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	var body showIssueClaimBody
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

func testShowIssueClaimRefreshStatusErrorDoesNotHotLoop(t *testing.T, status int) {
	t.Helper()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	pending := enqueueShowPendingClaim(t, spoke.DB, project.ID, issue.ShortID, "pending-cli", time.Now().UTC())
	var calls int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "claim refresh rejected", status)
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.Len(t, body.PendingClaims, 1)
	assert.Equal(t, pending.RequestUID, body.PendingClaims[0].RequestUID)
	require.NotNil(t, body.PendingClaims[0].LastError)
	assert.Contains(t, *body.PendingClaims[0].LastError, "status refresh")
	assert.Contains(t, *body.PendingClaims[0].LastError, http.StatusText(status))

	body = getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.Len(t, body.PendingClaims, 1)
	assert.Equal(t, 1, calls)
}

func testShowIssueClaimRefreshStatusErrorWithoutPendingDoesNotHotLoop(t *testing.T, status int) {
	t.Helper()
	ctx := context.Background()
	spoke := testenv.New(t)
	project, issue := createShowClaimSpokeProject(t, spoke)
	cachedAt := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	require.NoError(t, spoke.DB.ApplyClaimStatus(ctx, project.ID, issue.UID, db.ClaimStatus{
		Held: true,
		Holder: db.ClaimPrincipal{
			HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
			Holder:            "cached-holder",
			ClientKind:        "cli",
		},
		Claim:  showIssueCachedClaim(issue, "cached-holder", cachedAt),
		HubNow: cachedAt,
	}))
	var calls int
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "claim refresh rejected", status)
	}))
	t.Cleanup(hub.Close)
	writeShowClaimSpokeBinding(t, spoke, project, hub.URL, "claim-token", "")

	body := getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	body = getShowIssueClaimBody(t, spoke, project.ID, issue.ShortID)
	require.NotNil(t, body.Claim)
	assert.Equal(t, "cached-holder", body.Claim.Holder)
	assert.Equal(t, 1, calls)
}

func enqueueShowPendingClaim(
	t *testing.T,
	store *sqlitestore.Store,
	projectID int64,
	ref string,
	holder string,
	at time.Time,
) db.PendingClaimRequest {
	t.Helper()
	pending, err := store.EnqueuePendingClaim(context.Background(), db.PendingClaimParams{
		ProjectID: projectID,
		IssueRef:  ref,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: store.InstanceUID(),
			Holder:            holder,
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       at,
	})
	require.NoError(t, err)
	return pending
}

func assertShowLiveClaimCount(t *testing.T, store *sqlitestore.Store, issueUID string, want int) {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRow(
		`SELECT COUNT(*) FROM issue_claims WHERE issue_uid = ? AND released_at IS NULL`, issueUID,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func assertShowEventCount(t *testing.T, store *sqlitestore.Store, eventType string, want int) {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRow(
		`SELECT COUNT(*) FROM events WHERE type = ?`, eventType,
	).Scan(&got))
	assert.Equal(t, want, got)
}

func createShowClaimSpokeProject(t *testing.T, env *testenv.Env) (db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "spoke issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	return project, issue
}

func writeShowClaimSpokeBinding(
	t *testing.T,
	env *testenv.Env,
	project db.Project,
	hubURL string,
	token string,
	capabilities string,
) {
	t.Helper()
	_, err := env.DB.UpsertFederationBinding(context.Background(), db.FederationBinding{
		ProjectID:     project.ID,
		Role:          db.FederationRoleSpoke,
		HubURL:        hubURL,
		HubProjectID:  42,
		HubProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4AA",
		Enabled:       true,
	})
	require.NoError(t, err)
	require.NoError(t, config.WriteFederationCredential(project.UID, config.FederationCredential{
		HubURL:       hubURL,
		HubProjectID: 42,
		Token:        token,
		Capabilities: capabilities,
	}))
}

func showIssueClaimOut(issue db.Issue, holder string, at time.Time) *api.IssueClaimOut {
	return &api.IssueClaimOut{
		ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CB",
		ProjectID:         42,
		IssueUID:          issue.UID,
		Holder:            holder,
		HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4EA",
		ClientKind:        "cli",
		ClaimKind:         "hard",
		AcquiredAt:        at,
		Revision:          1,
		UpdatedAt:         at,
	}
}

func showIssueCachedClaim(issue db.Issue, holder string, at time.Time) *db.IssueClaim {
	return &db.IssueClaim{
		ClaimUID:          "01HZNQ7VFPK1XGD8R5MABCD4CC",
		ProjectID:         issue.ProjectID,
		IssueID:           issue.ID,
		IssueUID:          issue.UID,
		Holder:            holder,
		HolderInstanceUID: "01HZNQ7VFPK1XGD8R5MABCD4CD",
		ClientKind:        "cli",
		ClaimKind:         "hard",
		AcquiredAt:        at,
		Revision:          1,
		UpdatedAt:         at,
	}
}
