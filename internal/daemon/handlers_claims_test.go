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

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	katauid "go.kenn.io/kata/internal/uid"
)

const claimTestOtherSpokeUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"

func TestClaimAuthLocalDaemonBearerCanClaimHubProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "local-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, bearer("admin-token"), &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Granted)
	require.NotNil(t, out.Claim)
	assert.Equal(t, env.DB.InstanceUID(), out.Holder.HolderInstanceUID)
	assert.Equal(t, "local-cli", out.Holder.Holder)
}

func TestClaimRoutesRejectArchivedFederatedProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)
	_, _, err := env.DB.RemoveProject(context.Background(), db.RemoveProjectParams{
		ProjectID: project.ID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	headers := bearer("admin-token")
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "local-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, headers, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, headers)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
}

func TestClaimAuthIdentityTokenCanUseLocalLeaseRoutes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, issue := createClaimHubIssue(t, env)
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	headers := bearer("alice-token")
	body := map[string]any{
		"holder":      "alice-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}
	var acquired claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", body, headers, &acquired)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, acquired.Granted)
	assert.Equal(t, env.DB.InstanceUID(), acquired.Holder.HolderInstanceUID)
	assert.Equal(t, "alice", acquired.Holder.Holder)

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, headers)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	var released claimResponseBody
	resp = claimPost(t, env, project.ID, issue.ShortID, "release", body, headers, &released)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, released.Granted)

	resp = claimPost(t, env, project.ID, issue.ShortID, "claim", body, headers, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var forced claimResponseBody
	resp = claimPost(t, env, project.ID, issue.ShortID, "force_release",
		map[string]any{"actor": "alice", "reason": "operator"}, headers, &forced)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, forced.Granted)
}

func TestClaimAuthIdentityModeBootstrapTokenCannotMutateLeases(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, issue := createClaimHubIssue(t, env)
	headers := bearer("bootstrap-token")

	for _, action := range []string{"claim", "renew", "release", "force_release"} {
		t.Run(action, func(t *testing.T) {
			resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, action),
				map[string]any{
					"holder":      "bootstrap-cli",
					"client_kind": "cli",
					"claim_kind":  "hard",
					"actor":       "bootstrap",
				}, headers)
			assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "bootstrap_token_write_forbidden")
		})
	}
}

func TestClaimAuthEnrollmentTokenWithClaimCanClaimHubProject(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)
	created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, bearer(created.Token), &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Granted)
	require.NotNil(t, out.Claim)
	assert.Equal(t, federationTestSpokeUID, out.Holder.HolderInstanceUID)
	assert.Equal(t, "spoke-cli", out.Holder.Holder)
}

func TestClaimAuthEnrollmentTokenWinsBeforeAuthDisabledLocalFallback(t *testing.T) {
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)
	created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, bearer(created.Token), &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Granted)
	assert.Equal(t, federationTestSpokeUID, out.Holder.HolderInstanceUID)
	assert.NotEqual(t, env.DB.InstanceUID(), out.Holder.HolderInstanceUID)
}

func TestClaimAuthInsecureReadonlyEnrollmentTokenCanClaimHubProject(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	project, issue := createClaimHubIssue(t, env)
	created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, bearer(created.Token), &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Granted)
	assert.Equal(t, federationTestSpokeUID, out.Holder.HolderInstanceUID)
}

func TestClaimAuthEnrollmentTokenWithoutClaimIsForbidden(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)
	created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "pull")

	resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "claim"),
		map[string]any{"holder": "spoke-cli", "client_kind": "cli", "claim_kind": "hard"}, bearer(created.Token))

	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "auth_invalid")
}

func TestClaimAuthCallerSuppliedHolderInstanceUIDIsIgnored(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":              "local-cli",
		"holder_instance_uid": claimTestOtherSpokeUID,
		"client_kind":         "cli",
		"claim_kind":          "hard",
	}, bearer("admin-token"), &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, env.DB.InstanceUID(), out.Holder.HolderInstanceUID)
	assert.NotEqual(t, claimTestOtherSpokeUID, out.Holder.HolderInstanceUID)
}

func TestClaimAuthInsecureReadonlyRejectsUnauthenticatedClaimPost(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	project, issue := createClaimHubIssue(t, env)

	resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "claim"),
		map[string]any{"holder": "local-cli", "client_kind": "cli", "claim_kind": "hard"}, nil)

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "auth_required")
}

func TestClaimAuthInsecureReadonlyRejectsUnauthenticatedClaimStatusRead(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	project, issue := createClaimHubIssue(t, env)
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "local-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "auth_required")
}

func TestClaimAuthInsecureReadonlyClaimStatusRejectsStaleAuthorizationHeader(t *testing.T) {
	env := testenv.New(t, testenv.WithInsecureReadonly())
	project, issue := createClaimHubIssue(t, env)
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "local-cli",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer("stale-token"))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "auth_invalid")
}

func TestClaimAuthStatusEndpointReturnsLiveClaimAndHubNow(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)
	claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "local-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, bearer("admin-token"), nil)

	var out claimStatusBody
	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer("admin-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &out))

	assert.True(t, out.Held)
	assert.Equal(t, "local-cli", out.Holder.Holder)
	assert.Equal(t, env.DB.InstanceUID(), out.Holder.HolderInstanceUID)
	assert.False(t, out.HubNow.IsZero())
	require.NotNil(t, out.Claim)
	assert.Equal(t, issue.UID, out.Claim.IssueUID)
}

func TestClaimAuthForceReleaseRejectsEnrollmentAndUsesAdminAuth(t *testing.T) {
	ctx := context.Background()

	t.Run("local no-token deployment may force release", func(t *testing.T) {
		env := testenv.New(t)
		project, issue := createClaimHubIssue(t, env)
		created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")
		_, err := env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: federationTestSpokeUID,
				Holder:            "spoke-cli",
				ClientKind:        "cli",
			},
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "force_release"),
			map[string]any{"actor": "admin"}, nil)
		assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

		resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "force_release"),
			map[string]any{"actor": "admin"}, bearer(created.Token))
		assert.Equal(t, http.StatusConflict, resp.StatusCode, "after release should reach handler, not auth: %s", raw)
	})

	t.Run("token deployment requires admin bearer and rejects enrollment", func(t *testing.T) {
		env := testenv.New(t, testenv.WithAuthToken("admin-token"))
		project, issue := createClaimHubIssue(t, env)
		created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")
		_, err := env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: project.ID,
			IssueRef:  issue.ShortID,
			Principal: db.ClaimPrincipal{
				HolderInstanceUID: federationTestSpokeUID,
				Holder:            "spoke-cli",
				ClientKind:        "cli",
			},
			ClaimKind: "hard",
			Now:       time.Now().UTC(),
		})
		require.NoError(t, err)

		resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "force_release"),
			map[string]any{"actor": "admin"}, nil)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))

		resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "force_release"),
			map[string]any{"actor": "admin"}, bearer(created.Token))
		assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))

		resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "force_release"),
			map[string]any{"actor": "admin"}, bearer("admin-token"))
		assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	})
}

func TestClaimAuthStatusReadsStillRequireAuthPolicyOrClaimEnrollment(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project, issue := createClaimHubIssue(t, env)
	claimToken := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")
	pullToken := createClaimEnrollment(t, env, project.ID, claimTestOtherSpokeUID, "pull")
	otherProject := createFederatedHubProject(t, env, "other")
	otherToken := createClaimEnrollment(t, env, otherProject.ID, claimTestOtherSpokeUID, "claim")

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(raw))

	resp, raw = envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer(pullToken.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))

	resp, raw = envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer(otherToken.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))

	resp, raw = envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer(claimToken.Token))
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	resp, raw = envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, bearer("admin-token"))
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimAuthEnrollmentTokenDoesNotAuthorizeNonClaimRoutes(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	project := createFederatedHubProject(t, env, "hub")
	created := createClaimEnrollment(t, env, project.ID, federationTestSpokeUID, "claim")

	resp, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/projects", nil, bearer(created.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))

	resp, raw = envDoRaw(t, env, http.MethodGet, "/api/v1/projects", nil, bearer("admin-token"))
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func TestClaimActionGrantDenyRenewReleaseAndStatus(t *testing.T) {
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)

	var acquired claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "agent-a",
		"client_kind": "cli",
		"claim_kind":  "timed",
		"ttl_seconds": int64(120),
		"purpose":     "edit",
	}, nil, &acquired)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, acquired.Granted)
	require.NotNil(t, acquired.Claim)
	assert.Equal(t, "timed", acquired.Claim.ClaimKind)
	assert.Equal(t, "edit", acquired.Claim.Purpose)
	require.NotNil(t, acquired.Claim.ExpiresAt)
	require.NotNil(t, acquired.Event)
	assert.Equal(t, "claim.acquired", acquired.Event.Type)

	var denied claimResponseBody
	resp = claimPost(t, env, project.ID, issue.ShortID, "claim",
		map[string]any{"holder": "agent-b", "client_kind": "cli", "claim_kind": "hard"}, nil, &denied)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, denied.Granted)
	assert.Equal(t, "agent-a", denied.Holder.Holder)
	require.NotNil(t, denied.Claim)
	assert.Equal(t, acquired.Claim.ClaimUID, denied.Claim.ClaimUID)

	var status claimStatusBody
	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &status))
	assert.True(t, status.Held)
	assert.Equal(t, "agent-a", status.Holder.Holder)
	assert.False(t, status.HubNow.IsZero())

	resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "renew"),
		map[string]any{"holder": "agent-b", "client_kind": "cli", "ttl_seconds": int64(300)}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_not_held")

	var renewed claimResponseBody
	resp = claimPost(t, env, project.ID, issue.ShortID, "renew", map[string]any{
		"holder":      "agent-a",
		"client_kind": "cli",
		"ttl_seconds": int64(300),
	}, nil, &renewed)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, renewed.Claim)
	assert.Equal(t, acquired.Claim.ClaimUID, renewed.Claim.ClaimUID)
	require.NotNil(t, renewed.Claim.ExpiresAt)
	assert.True(t, renewed.Claim.ExpiresAt.After(*acquired.Claim.ExpiresAt))
	assert.Nil(t, renewed.Event, "renew is response-only state and intentionally does not emit claim.renewed")

	resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "release"),
		map[string]any{"holder": "agent-b", "client_kind": "cli"}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_not_held")

	var released claimResponseBody
	resp = claimPost(t, env, project.ID, issue.ShortID, "release", map[string]any{
		"holder":      "agent-a",
		"client_kind": "cli",
		"reason":      "done",
	}, nil, &released)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, released.Claim)
	assert.NotNil(t, released.Claim.ReleasedAt)
	require.NotNil(t, released.Event)
	assert.Equal(t, "claim.released", released.Event.Type)

	resp, raw = envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	status = claimStatusBody{}
	require.NoError(t, json.Unmarshal(raw, &status))
	assert.False(t, status.Held)
	assert.Nil(t, status.Claim)
	assert.False(t, status.HubNow.IsZero())
}

func TestClaimActionValidationAndExpiredErrors(t *testing.T) {
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)

	resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "claim"),
		map[string]any{"holder": "agent-a", "client_kind": "cli", "claim_kind": "timed", "ttl_seconds": int64(59)}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")

	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	resp, raw = envDoRaw(t, env, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "renew"),
		map[string]any{"holder": "agent-a", "client_kind": "cli", "ttl_seconds": int64(300)}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "claim_expired")
}

func TestClaimActionAcquireBroadcastsOpportunisticExpiryBeforeAcquire(t *testing.T) {
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	var out claimResponseBody
	resp := claimPost(t, env, project.ID, issue.ShortID, "claim",
		map[string]any{"holder": "agent-b", "client_kind": "cli", "claim_kind": "hard"}, nil, &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	first := receiveMsg(t, sub.Ch, time.Second, "claim expired broadcast")
	second := receiveMsg(t, sub.Ch, time.Second, "claim acquired broadcast")
	require.NotNil(t, first.Event)
	require.NotNil(t, second.Event)
	assert.Equal(t, "claim.expired", first.Event.Type)
	assert.Equal(t, "claim.acquired", second.Event.Type)
	assert.Less(t, first.Event.ID, second.Event.ID)
}

func TestClaimActionBroadcastsOpportunisticExpiryOnExpiredClaimProject(t *testing.T) {
	env := testenv.New(t)
	projectA, issueA := createClaimHubIssue(t, env)
	projectB, issueB := createClaimHubIssueNamed(t, env, "hub-b")
	subA := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectA.ID})
	defer subA.Unsub()
	subB := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectB.ID})
	defer subB.Unsub()
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: projectB.ID,
		IssueRef:  issueB.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-b",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	var out claimResponseBody
	resp := claimPost(t, env, projectA.ID, issueA.ShortID, "claim",
		map[string]any{"holder": "agent-a", "client_kind": "cli", "claim_kind": "hard"}, nil, &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	expired := receiveMsg(t, subB.Ch, time.Second, "other project claim expired broadcast")
	require.NotNil(t, expired.Event)
	assert.Equal(t, "claim.expired", expired.Event.Type)
	assert.Equal(t, projectB.ID, expired.ProjectID)
	acquired := receiveMsg(t, subA.Ch, time.Second, "current project claim acquired broadcast")
	require.NotNil(t, acquired.Event)
	assert.Equal(t, "claim.acquired", acquired.Event.Type)
	assert.Equal(t, projectA.ID, acquired.ProjectID)
}

func TestClaimActionDoesNotBroadcastRolledBackOpportunisticExpiry(t *testing.T) {
	env := testenv.New(t)
	projectA, issueA := createClaimHubIssue(t, env)
	projectB, issueB := createClaimHubIssueNamed(t, env, "hub-b")
	subA := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectA.ID})
	defer subA.Unsub()
	subB := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: projectB.ID})
	defer subB.Unsub()
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: projectA.ID,
		IssueRef:  issueA.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-owner",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now(),
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: projectB.ID,
		IssueRef:  issueB.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-b",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	resp := claimPost(t, env, projectA.ID, issueA.ShortID, "claim",
		map[string]any{"holder": "agent-denied", "client_kind": "cli", "claim_kind": "hard"}, nil, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assertNoReceive(t, subA.Ch, 100*time.Millisecond, "denied claim should not broadcast rolled-back expiration")
	assertNoReceive(t, subB.Ch, 100*time.Millisecond, "denied claim should not broadcast rolled-back expiration")
}

func TestClaimStatusBroadcastsOpportunisticExpiry(t *testing.T) {
	env := testenv.New(t)
	project, issue := createClaimHubIssue(t, env)
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: env.DB.InstanceUID(),
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Minute,
		Now:       time.Now().Add(-2 * time.Minute),
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, claimStatusPath(project.ID, issue.ShortID), nil, nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	msg := receiveMsg(t, sub.Ch, time.Second, "claim expired broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "claim.expired", msg.Event.Type)
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "status should emit only one expiry")
}

func TestClaimActionForceReleaseDefaultAndExplicitReasons(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       map[string]any
		wantReason string
	}{
		{
			name:       "default",
			body:       map[string]any{"actor": "admin"},
			wantReason: "admin_force_release",
		},
		{
			name:       "explicit",
			body:       map[string]any{"actor": "admin", "reason": "stale holder"},
			wantReason: "stale holder",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			project, issue := createClaimHubIssue(t, env)
			_, err := env.DB.AcquireClaim(context.Background(), db.AcquireClaimParams{
				ProjectID: project.ID,
				IssueRef:  issue.ShortID,
				Principal: db.ClaimPrincipal{
					HolderInstanceUID: federationTestSpokeUID,
					Holder:            "agent-a",
					ClientKind:        "cli",
				},
				ClaimKind: "hard",
				Now:       time.Now().UTC(),
			})
			require.NoError(t, err)

			var out claimResponseBody
			resp := claimPost(t, env, project.ID, issue.ShortID, "force_release", tc.body, nil, &out)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NotNil(t, out.Claim)
			assert.NotNil(t, out.Claim.ReleasedAt)
			require.NotNil(t, out.Claim.ReleaseReason)
			assert.Equal(t, tc.wantReason, *out.Claim.ReleaseReason)
			require.NotNil(t, out.Event)
			assert.Equal(t, "claim.force_released", out.Event.Type)

			var payload struct {
				Reason string `json:"reason"`
			}
			require.NoError(t, json.Unmarshal([]byte(out.Event.Payload), &payload))
			assert.Equal(t, tc.wantReason, payload.Reason)
		})
	}
}

func TestClaimActionEmittedEventsBroadcastAndEnqueueHooks(t *testing.T) {
	hooksSink := &recordingSink{}
	bcast := daemon.NewEventBroadcaster()
	d := openTestDB(t)
	ts := startTestServer(t, daemon.ServerConfig{
		DB:          d.db,
		StartedAt:   d.now,
		Broadcaster: bcast,
		Hooks:       hooksSink,
	})
	project, issue := createClaimHubIssueInDB(t, d.db)
	sub := bcast.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()

	resp, raw := doReq(t, ts, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "claim"),
		map[string]any{"holder": "agent-a", "client_kind": "cli", "claim_kind": "hard"}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	msg := receiveMsg(t, sub.Ch, time.Second, "claim acquired broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "claim.acquired", msg.Event.Type)
	assert.Equal(t, project.ID, msg.ProjectID)

	captured := hooksSink.snapshot()
	require.Len(t, captured, 1)
	assert.Equal(t, "claim.acquired", captured[0].Type)

	resp, raw = doReq(t, ts, http.MethodPost, claimActionPath(project.ID, issue.ShortID, "release"),
		map[string]any{"holder": "agent-a", "client_kind": "cli", "reason": "done"}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	msg = receiveMsg(t, sub.Ch, time.Second, "claim released broadcast")
	require.NotNil(t, msg.Event)
	assert.Equal(t, "claim.released", msg.Event.Type)
	captured = hooksSink.snapshot()
	require.Len(t, captured, 2)
	assert.Equal(t, "claim.released", captured[1].Type)
}

func TestClaimForwardAcquireAndReleaseUpdatesSpokeCache(t *testing.T) {
	ctx := context.Background()
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")

	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))

	var acquired claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
		"purpose":     "edit",
	}, nil, &acquired)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, acquired.Granted)
	require.NotNil(t, acquired.Claim)

	cached, err := spoke.DB.ClaimStatus(ctx, spokeProject.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, cached.Held)
	assert.Equal(t, "spoke-cli", cached.Holder.Holder)
	assert.Equal(t, spoke.DB.InstanceUID(), cached.Holder.HolderInstanceUID)

	var released claimResponseBody
	resp = claimPost(t, spoke, spokeProject.ID, issue.ShortID, "release", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"reason":      "done",
	}, nil, &released)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, released.Granted)

	cached, err = spoke.DB.ClaimStatus(ctx, spokeProject.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, cached.Held)
}

func TestClaimStatusForwardRefreshesSpokeCache(t *testing.T) {
	ctx := context.Background()
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: claimTestOtherSpokeUID,
			Holder:            "other-spoke",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	var status claimStatusBody
	resp, raw := envDoRaw(t, spoke, http.MethodGet, claimStatusPath(spokeProject.ID, issue.ShortID), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &status))
	require.True(t, status.Held)
	assert.Equal(t, "other-spoke", status.Holder.Holder)

	cached, err := spoke.DB.ClaimStatus(ctx, spokeProject.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, cached.Held)
	assert.Equal(t, "other-spoke", cached.Holder.Holder)
	assert.Equal(t, claimTestOtherSpokeUID, cached.Holder.HolderInstanceUID)
}

func TestClaimForwardDeniedAcquireCachesHubHolder(t *testing.T) {
	ctx := context.Background()
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	_, err := hub.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: hubProject.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: claimTestOtherSpokeUID,
			Holder:            "other-spoke",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Now:       time.Now().UTC(),
	})
	require.NoError(t, err)

	var denied claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &denied)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.False(t, denied.Granted)
	assert.Equal(t, "other-spoke", denied.Holder.Holder)

	cached, err := spoke.DB.ClaimStatus(ctx, spokeProject.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, cached.Held)
	assert.Equal(t, "other-spoke", cached.Holder.Holder)
	assert.Equal(t, claimTestOtherSpokeUID, cached.Holder.HolderInstanceUID)
}

func TestClaimForwardCrossOriginRedirectDoesNotReachRedirectTarget(t *testing.T) {
	_, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	var (
		redirectTargetHit  bool
		redirectTargetAuth string
	)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHit = true
		redirectTargetAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(redirectTarget.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+r.URL.Path, http.StatusTemporaryRedirect) //nolint:gosec // test server intentionally redirects to another test server.
	}))
	t.Cleanup(redirector.Close)
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       redirector.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))

	var out claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "redirect-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Pending)
	assert.False(t, redirectTargetHit)
	assert.Empty(t, redirectTargetAuth)
}

func TestPendingClaimOfflineAcquireEnqueuesAndReleaseDoesNotClearCache(t *testing.T) {
	ctx := context.Background()
	hub, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       hub.URL,
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))

	var acquired claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "spoke-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &acquired)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, acquired.Granted)

	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       "http://127.0.0.1:1",
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	resp, raw := envDoRaw(t, spoke, http.MethodPost, claimActionPath(spokeProject.ID, issue.ShortID, "release"),
		map[string]any{"holder": "spoke-cli", "client_kind": "cli", "reason": "done"}, nil)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(raw))
	cached, err := spoke.DB.ClaimStatus(ctx, spokeProject.ID, issue.ShortID, time.Now().UTC())
	require.NoError(t, err)
	require.True(t, cached.Held)

	var pending claimResponseBody
	resp = claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "offline-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &pending)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, pending.Pending)
	assert.False(t, pending.Granted)
	assert.NotEmpty(t, pending.RequestUID)
	assert.True(t, katauid.Valid(pending.RequestUID), "request_uid should be a valid UID")

	var n int
	require.NoError(t, spoke.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_claim_requests
		 WHERE issue_uid = ? AND holder = ? AND rejected_at IS NULL AND resolved_at IS NULL`,
		issue.UID, "offline-cli").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestPendingClaimOfflineAcquireDuplicateReturnsExistingRequest(t *testing.T) {
	ctx := context.Background()
	_, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       "http://127.0.0.1:1",
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))
	body := map[string]any{
		"holder":      "offline-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}

	var first claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", body, nil, &first)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.True(t, first.Pending)
	require.NotEmpty(t, first.RequestUID)

	var second claimResponseBody
	resp = claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", body, nil, &second)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, second.Pending)
	assert.Equal(t, first.RequestUID, second.RequestUID)

	var n int
	require.NoError(t, spoke.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_claim_requests
		 WHERE issue_uid = ? AND holder = ? AND holder_instance_uid = ? AND client_kind = ?
		   AND rejected_at IS NULL AND resolved_at IS NULL`,
		issue.UID, "offline-cli", spoke.DB.InstanceUID(), "cli").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestPendingClaimUnixHubOfflineAcquireEnqueues(t *testing.T) {
	ctx := context.Background()
	_, spoke, hubProject, spokeProject, issue, token := createClaimForwardingPair(t, "claim")
	require.NoError(t, config.WriteFederationCredential(spokeProject.UID, config.FederationCredential{
		HubURL:       "http://kata.invalid",
		HubProjectID: hubProject.ID,
		Token:        token,
		Capabilities: "claim",
	}))

	var pending claimResponseBody
	resp := claimPost(t, spoke, spokeProject.ID, issue.ShortID, "claim", map[string]any{
		"holder":      "offline-cli",
		"client_kind": "cli",
		"claim_kind":  "hard",
	}, nil, &pending)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, pending.Pending)
	assert.False(t, pending.Granted)
	assert.NotEmpty(t, pending.RequestUID)
	var n int
	require.NoError(t, spoke.DB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_claim_requests
		 WHERE issue_uid = ? AND holder = ? AND rejected_at IS NULL AND resolved_at IS NULL`,
		issue.UID, "offline-cli").Scan(&n))
	assert.Equal(t, 1, n)
}

type claimResponseBody struct {
	Granted    bool           `json:"granted"`
	Pending    bool           `json:"pending"`
	RequestUID string         `json:"request_uid"`
	Holder     claimPrincipal `json:"holder"`
	Claim      *claimOut      `json:"claim"`
	Event      *struct {
		Type    string `json:"type"`
		Actor   string `json:"actor"`
		Payload string `json:"payload"`
	} `json:"event,omitempty"`
}

type claimStatusBody struct {
	Held   bool           `json:"held"`
	Holder claimPrincipal `json:"holder"`
	Claim  *claimOut      `json:"claim"`
	HubNow time.Time      `json:"hub_now"`
}

type claimPrincipal struct {
	HolderInstanceUID string `json:"holder_instance_uid"`
	Holder            string `json:"holder"`
	ClientKind        string `json:"client_kind"`
}

type claimOut struct {
	ClaimUID          string     `json:"claim_uid"`
	IssueUID          string     `json:"issue_uid"`
	Holder            string     `json:"holder"`
	HolderInstanceUID string     `json:"holder_instance_uid"`
	ClientKind        string     `json:"client_kind"`
	Purpose           string     `json:"purpose"`
	ClaimKind         string     `json:"claim_kind"`
	ExpiresAt         *time.Time `json:"expires_at"`
	ReleasedAt        *time.Time `json:"released_at"`
	ReleaseReason     *string    `json:"release_reason"`
	Revision          int64      `json:"revision"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func createClaimHubIssue(t *testing.T, env *testenv.Env) (db.Project, db.Issue) {
	t.Helper()
	return createClaimHubIssueNamed(t, env, "hub")
}

func createClaimHubIssueNamed(t *testing.T, env *testenv.Env, name string) (db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	project := createFederatedHubProject(t, env, name)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)
	return project, issue
}

func createClaimHubIssueInDB(t *testing.T, store *sqlitestore.Store) (db.Project, db.Issue) {
	t.Helper()
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claim target",
		Author:    "tester",
	})
	require.NoError(t, err)
	return project, issue
}

func createClaimForwardingPair(
	t *testing.T,
	capabilities string,
) (*testenv.Env, *testenv.Env, db.Project, db.Project, db.Issue, string) {
	t.Helper()
	ctx := context.Background()
	hub := testenv.New(t)
	hubProject, issue := createClaimHubIssue(t, hub)
	spoke := testenv.New(t)
	spokeProject, err := spoke.DB.CreateProjectWithUID(ctx, hubProject.Name, hubProject.UID)
	require.NoError(t, err)
	_, _, err = spoke.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       spokeProject.ID,
		Title:           issue.Title,
		Author:          "tester",
		UID:             issue.UID,
		ShortIDOverride: issue.ShortID,
	})
	require.NoError(t, err)
	_, err = spoke.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            spokeProject.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               hub.URL,
		HubProjectID:         hubProject.ID,
		HubProjectUID:        hubProject.UID,
		ReplayHorizonEventID: 1,
		Enabled:              true,
	})
	require.NoError(t, err)
	created := createClaimEnrollment(t, hub, hubProject.ID, spoke.DB.InstanceUID(), capabilities)
	return hub, spoke, hubProject, spokeProject, issue, created.Token
}

func createClaimEnrollment(
	t *testing.T,
	env *testenv.Env,
	projectID int64,
	spokeUID string,
	capabilities string,
) db.CreatedFederationEnrollment {
	t.Helper()
	created, err := env.DB.CreateFederationEnrollment(context.Background(), db.CreateFederationEnrollmentParams{
		Token:            spokeUID + "-" + capabilities + "-token",
		SpokeInstanceUID: spokeUID,
		ProjectID:        &projectID,
		Capabilities:     capabilities,
	})
	require.NoError(t, err)
	return created
}

func claimPost(
	t *testing.T,
	env *testenv.Env,
	projectID int64,
	ref string,
	action string,
	body any,
	headers map[string]string,
	out any,
) *http.Response {
	t.Helper()
	resp, raw := envDoRaw(t, env, http.MethodPost, claimActionPath(projectID, ref, action), body, headers)
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		require.NoError(t, json.Unmarshal(raw, out), string(raw))
	}
	return resp
}

func claimActionPath(projectID int64, ref, action string) string {
	if action == "claim" {
		action = "acquire"
	}
	return issuePathRef(projectID, ref, "lease/actions/"+action)
}

func claimStatusPath(projectID int64, ref string) string {
	return issuePathRef(projectID, ref, "lease")
}
