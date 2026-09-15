package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCreateTokenEndpoint_BootstrapCreatesToken(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm", "name": "laptop"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	assert.NotContains(t, string(bs), "token_hash")

	var out struct {
		Token struct {
			ID    int64   `json:"id"`
			Actor string  `json:"actor"`
			Name  *string `json:"name"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	assert.Equal(t, "wesm", out.Token.Actor)
	require.NotNil(t, out.Token.Name)
	assert.Equal(t, "laptop", *out.Token.Name)
	assert.True(t, strings.HasPrefix(out.Plaintext, "kata_"))
	assert.Equal(t, 1, strings.Count(string(bs), out.Plaintext))

	resolved, err := env.DB.ResolveAPIToken(context.Background(), out.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, out.Token.ID, resolved.ID)
	assert.Equal(t, "wesm", resolved.Actor)
}

func TestCreateTokenEndpoint_DBTokenForbidden(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	_, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"},
		map[string]string{"Authorization": "Bearer user-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusForbidden, "token_admin_forbidden")
}

func TestListTokenEndpoint_RedactsPlaintextAndHash(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	createResp, createBody := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm", "name": "laptop"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equal(t, http.StatusOK, createResp.StatusCode)
	var created struct {
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(createBody, &created))

	resp, bs := envDoRaw(t, env, http.MethodGet, "/api/v1/tokens", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	assert.NotContains(t, string(bs), "token_hash")
	assert.NotContains(t, string(bs), created.Plaintext)
	var out struct {
		Tokens []struct {
			Actor string  `json:"actor"`
			Name  *string `json:"name"`
		} `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.Len(t, out.Tokens, 1)
	assert.Equal(t, "wesm", out.Tokens[0].Actor)
}

func TestListTokenEndpointReportsLifecycleAtOneServerObservationTime(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	scope := &db.APITokenScope{
		Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
	}
	now := time.Now().UTC()
	liveExpiry := now.Add(time.Hour)
	expiredExpiry := now.Add(-time.Hour)
	for _, token := range []db.CreateAPITokenParams{
		{PlaintextToken: "unscoped-token", Actor: "unscoped", AdminActor: db.BootstrapActor},                                        //nolint:gosec // Deterministic test credential.
		{PlaintextToken: "live-token", Actor: "live", AdminActor: db.BootstrapActor, Scope: scope, ExpiresAt: &liveExpiry},          //nolint:gosec // Deterministic test credential.
		{PlaintextToken: "expired-token", Actor: "expired", AdminActor: db.BootstrapActor, Scope: scope, ExpiresAt: &expiredExpiry}, //nolint:gosec // Deterministic test credential.
		{PlaintextToken: "revoked-token", Actor: "revoked", AdminActor: db.BootstrapActor, Scope: scope, ExpiresAt: &expiredExpiry}, //nolint:gosec // Deterministic test credential.
	} {
		_, _, createErr := env.DB.CreateAPIToken(ctx, token)
		require.NoError(t, createErr)
	}
	tokens, err := env.DB.ListAPITokens(ctx)
	require.NoError(t, err)
	var revokedID int64
	for _, token := range tokens {
		if token.Actor == "revoked" {
			revokedID = token.ID
		}
	}
	require.NotZero(t, revokedID)
	_, _, err = env.DB.RevokeAPIToken(ctx, revokedID, db.BootstrapActor)
	require.NoError(t, err)

	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/tokens", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(body))
	var out struct {
		ObservedAt time.Time `json:"observed_at"`
		Tokens     []struct {
			Actor string `json:"actor"`
			State string `json:"state"`
		} `json:"tokens"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	assert.WithinDuration(t, time.Now().UTC(), out.ObservedAt, 2*time.Second)
	states := make(map[string]string, len(out.Tokens))
	for _, token := range out.Tokens {
		states[token.Actor] = token.State
	}
	assert.Equal(t, map[string]string{
		"unscoped": "live", "live": "live", "expired": "expired", "revoked": "revoked",
	}, states)
}

func TestRevokeTokenEndpoint_AppendsRevocation(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	tok, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost,
		"/api/v1/tokens/"+strconv.FormatInt(tok.ID, 10)+"/actions/revoke", nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var out struct {
		Token struct {
			RevokedAt *string `json:"revoked_at"`
		} `json:"token"`
		Event struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Token.RevokedAt)
	assert.Equal(t, "token.revoked", out.Event.Type)
	assert.Equal(t, db.BootstrapActor, out.Event.Actor)
	_, err = env.DB.ResolveAPIToken(context.Background(), "user-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestCreateTokenEndpoint_RejectsReservedBootstrapActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "Bootstrap"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusBadRequest, "validation")
}

func TestCreateTokenEndpoint_StaticModeRejectsUnusableIdentityToken(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"))
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"},
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "token_identity_mode_required")
	assert.Contains(t, string(bs), "require_token_identity")

	var tokens int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM api_tokens`).Scan(&tokens))
	assert.Zero(t, tokens)
	var events int
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE type = 'token.created'`).Scan(&events))
	assert.Zero(t, events)
}

func TestCreateTokenEndpoint_LocalNoAuthCanAdmin(t *testing.T) {
	env := testenv.New(t)
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens",
		map[string]string{"actor": "wesm"}, nil)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))

	var out struct {
		Token struct {
			ID int64 `json:"id"`
		} `json:"token"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	var actor string
	require.NoError(t, env.DB.QueryRowContext(context.Background(),
		`SELECT actor FROM events WHERE type = 'token.created' AND json_extract(payload, '$.token_id') = ?`,
		out.Token.ID).Scan(&actor))
	assert.Equal(t, db.BootstrapActor, actor)
}

func TestCreateTokenEndpoint_CreatesIssueSubtreeGrant(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)

	before := time.Now().UTC()
	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens", map[string]any{
		"actor": "worker-a",
		"scope": map[string]any{
			"kind": "issue_subtree", "project_uid": project.UID, "root_issue_uid": root.UID,
		},
		"expires_in_seconds": 3600,
	}, map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))

	var out struct {
		Token struct {
			Scope     *db.APITokenScope `json:"scope"`
			ExpiresAt *time.Time        `json:"expires_at"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	require.NoError(t, json.Unmarshal(bs, &out))
	require.NotNil(t, out.Token.Scope)
	assert.Equal(t, db.APITokenScopeIssueSubtree, out.Token.Scope.Kind)
	assert.Equal(t, project.UID, out.Token.Scope.ProjectUID)
	assert.Equal(t, root.UID, out.Token.Scope.RootIssueUID)
	require.NotNil(t, out.Token.ExpiresAt)
	assert.WithinDuration(t, before.Add(time.Hour), *out.Token.ExpiresAt, 2*time.Second)

	resolved, err := env.DB.ResolveAPIToken(ctx, out.Plaintext)
	require.NoError(t, err)
	require.NotNil(t, resolved.Scope)
	assert.Equal(t, *out.Token.Scope, *resolved.Scope)

	resp, bs = envDoRaw(t, env, http.MethodGet, "/api/v1/instance", nil,
		map[string]string{"Authorization": "Bearer " + out.Plaintext})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", string(bs))
	var instance struct {
		Auth struct {
			Scope                 *db.APITokenScope `json:"scope"`
			ExpiresAt             *time.Time        `json:"expires_at"`
			AllowedActions        []string          `json:"allowed_actions"`
			CloseRequiresEvidence bool              `json:"close_requires_evidence"`
		} `json:"auth"`
	}
	require.NoError(t, json.Unmarshal(bs, &instance))
	require.Equal(t, out.Token.Scope, instance.Auth.Scope)
	require.Equal(t, out.Token.ExpiresAt, instance.Auth.ExpiresAt)
	require.Equal(t, []string{
		"issue.read", "issue.create_child", "issue.edit", "issue.comment", "issue.labels",
		"issue.metadata", "issue.assign", "issue.claim", "issue.close", "issue.reopen",
		"issue.link", "issue.lease", "activity.read",
	}, instance.Auth.AllowedActions)
	require.True(t, instance.Auth.CloseRequiresEvidence)
}

func TestCreateTokenEndpoint_RejectsIncompleteOrMismatchedIssueScope(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)

	tests := []struct {
		name string
		body map[string]any
		code string
	}{
		{name: "missing expiry", body: map[string]any{"actor": "worker-a", "scope": map[string]any{
			"kind": "issue_subtree", "project_uid": project.UID, "root_issue_uid": root.UID,
		}}, code: "invalid_token_scope"},
		{name: "unknown kind", body: map[string]any{"actor": "worker-a", "scope": map[string]any{
			"kind": "unknown", "project_uid": project.UID, "root_issue_uid": root.UID,
		}, "expires_in_seconds": 60}, code: "validation"},
		{name: "project mismatch", body: map[string]any{"actor": "worker-a", "scope": map[string]any{
			"kind": "issue_subtree", "project_uid": other.UID, "root_issue_uid": root.UID,
		}, "expires_in_seconds": 60}, code: "invalid_token_scope"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens", tc.body,
				map[string]string{"Authorization": "Bearer bootstrap-token"})
			assertAPIError(t, resp.StatusCode, body, http.StatusBadRequest, tc.code)
		})
	}
}

func TestCreateTokenEndpoint_ScopedGrantRequiresIdentityMode(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens", map[string]any{
		"actor": "worker-a", "scope": map[string]any{
			"kind": "issue_subtree", "project_uid": project.UID, "root_issue_uid": root.UID,
		}, "expires_in_seconds": 60,
	}, nil)
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "token_identity_mode_required")
}

func TestCreateTokenEndpoint_RejectsSpokeReplicaRoot(t *testing.T) {
	ctx := context.Background()
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	root, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 42, HubProjectUID: project.UID,
		ReplayHorizonEventID: 1, Enabled: true,
	})
	require.NoError(t, err)

	resp, bs := envDoRaw(t, env, http.MethodPost, "/api/v1/tokens", map[string]any{
		"actor": "worker-a", "scope": map[string]any{
			"kind": "issue_subtree", "project_uid": project.UID, "root_issue_uid": root.UID,
		}, "expires_in_seconds": 60,
	}, map[string]string{"Authorization": "Bearer bootstrap-token"})
	assertAPIError(t, resp.StatusCode, bs, http.StatusConflict, "scoped_token_spoke_forbidden")
	assert.Contains(t, string(bs), "authoritative hub")
}
