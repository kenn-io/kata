package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

const federationTestSpokeUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"

func TestFederationEnableAndMetadata(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "baseline",
		Author:    "tester",
	})
	require.NoError(t, err)

	var enabled api.ProjectFederationBody
	resp := envDoJSON(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/enable",
		map[string]any{"actor": "tester"}, &enabled)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, project.ID, enabled.ProjectID)
	assert.Equal(t, project.UID, enabled.ProjectUID)
	assert.Equal(t, project.Name, enabled.ProjectName)
	assert.Greater(t, enabled.ReplayHorizonEventID, int64(0))
	assert.GreaterOrEqual(t, enabled.BaselineThroughEventID, enabled.ReplayHorizonEventID)
	assertFederationEventCount(t, env.DB, "project.federation_enabled", 1)
	assertFederationEventCount(t, env.DB, "issue.snapshot", 1)

	var enabledAgain api.ProjectFederationBody
	resp = envDoJSON(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/enable",
		map[string]any{"actor": "tester"}, &enabledAgain)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, enabled, enabledAgain)
	assertFederationEventCount(t, env.DB, "project.federation_enabled", 1)
	assertFederationEventCount(t, env.DB, "issue.snapshot", 1)

	var got api.ProjectFederationBody
	envGetJSON(t, env, projectPath(project.ID)+"/federation", &got)
	assert.Equal(t, enabled, got)
}

func TestFederationEnableIdentityModeBootstrapTokenCannotWrite(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/enable",
		map[string]any{"actor": "spoofed"},
		bearer("bootstrap-token"))

	assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "bootstrap_token_write_forbidden")
	assertFederationEventCount(t, env.DB, "project.federation_enabled", 0)
}

func TestFederationEnableIdentityModeUsesDBTokenActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/enable",
		map[string]any{"actor": "spoofed"},
		bearer("alice-token"))

	require.Equal(t, http.StatusOK, resp.StatusCode, "response: %s", raw)
	var actor string
	require.NoError(t, env.DB.QueryRow(`
		SELECT actor
		  FROM events
		 WHERE project_id = ? AND type = 'project.federation_enabled'`,
		project.ID).Scan(&actor))
	assert.Equal(t, "alice", actor)
}

func TestFederationMetadataRecoversBaselineAfterPurgeReset(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "hub")
	require.NoError(t, err)
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "purged issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	purgeLog, err := env.DB.PurgeIssue(ctx, issue.ID, "tester", nil)
	require.NoError(t, err)
	require.NotNil(t, purgeLog.PurgeResetAfterEventID)

	var got api.ProjectFederationBody
	envGetJSON(t, env, projectPath(project.ID)+"/federation", &got)

	assert.Greater(t, got.ReplayHorizonEventID, *purgeLog.PurgeResetAfterEventID)
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, got.ReplayHorizonEventID, binding.ReplayHorizonEventID)
}

func TestFederationReplicaCreatesProjectAndBinding(t *testing.T) {
	env := testenv.New(t)

	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "hub-token",
	}, &out)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.Equal(t, "hub", out.Project.Name)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", out.Project.UID)
	assert.Equal(t, string(db.FederationRoleSpoke), out.Binding.Role)
	assert.Equal(t, int64(42), out.Binding.HubProjectID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", out.Binding.HubProjectUID)
	assert.Equal(t, int64(9), out.Binding.ReplayHorizonEventID)
	assert.Equal(t, int64(8), out.Binding.PullCursorEventID)

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "hub-token", creds.Projects[out.Project.UID].Token)
	assert.Equal(t, "http://127.0.0.1:7373", creds.Projects[out.Project.UID].HubURL)
	assert.Equal(t, int64(42), creds.Projects[out.Project.UID].HubProjectID)
}

func TestFederationReplicaSetupIsIdempotentAndUsesJSONTags(t *testing.T) {
	env := testenv.New(t)
	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
	}
	var first api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &first)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", body, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(raw), `"role":"spoke"`)
	assert.Contains(t, string(raw), `"hub_project_id":42`)
	assert.NotContains(t, string(raw), `"Role"`)
	assert.NotContains(t, string(raw), `"HubProjectID"`)

	projects, err := env.DB.ListProjects(context.Background())
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	binding, err := env.DB.FederationBindingByProject(context.Background(), first.Project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(8), binding.PullCursorEventID)
}

func TestFederationReplicaSetupRejectsIncompatibleRetry(t *testing.T) {
	env := testenv.New(t)
	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "original-token",
	}
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body["hub_project_id"] = int64(43)
	body["token"] = "wrong-token"

	resp = envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "original-token", creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"].Token)
	assert.Equal(t, int64(42), creds.Projects["01HZNQ7VFPK1XGD8R5MABCD4EX"].HubProjectID)
}

func TestFederationReplicaSetupPushEnabledWritesCredentialAndEnablesPush(t *testing.T) {
	env := testenv.New(t)

	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
	}, &out)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Binding.PushEnabled)

	binding, err := env.DB.FederationBindingByProject(context.Background(), out.Project.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled)
	assert.Zero(t, binding.PushCursorEventID)

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "push-token", creds.Projects[out.Project.UID].Token)
}

func TestFederationReplicaSetupRejectsPushEnabledWithoutPushCapability(t *testing.T) {
	env := testenv.New(t)

	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "pull-token",
		"capabilities":            "pull",
		"push_enabled":            true,
	}, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestFederationReplicaSetupAdoptsExistingProject(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProjectWithUID(ctx, "spoke", "01HZNQ7VFPK1XGD8R5MABCD4EA")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local issue",
		Author:    "tester",
	})
	require.NoError(t, err)

	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}
	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Adopted)
	assert.Equal(t, int64(1), out.AdoptionSnapshotCount)
	assert.Equal(t, project.ID, out.Project.ID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", out.Project.UID)
	assert.True(t, out.Binding.PushEnabled)
	assert.Equal(t, int64(42), out.Binding.HubProjectID)

	adopted, err := env.DB.ProjectByID(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", adopted.UID)
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.True(t, binding.PushEnabled)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", binding.HubProjectUID)

	var retry api.CreateFederationReplicaBody
	resp = envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &retry)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, retry.Adopted)

	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "push-token", creds.Projects[out.Project.UID].Token)
}

func TestFederationReplicaSetupAdoptExistingBindsUnboundProjectWithHubUID(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProjectWithUID(ctx, "spoke", "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	_, _, err = env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "existing",
		Author:    "tester",
	})
	require.NoError(t, err)

	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, &out)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.True(t, out.Adopted)
	assert.Equal(t, int64(1), out.AdoptionSnapshotCount)
	assert.Equal(t, project.ID, out.Project.ID)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EX", out.Binding.HubProjectUID)
	assert.True(t, out.Binding.PushEnabled)
}

func TestFederationReplicaSetupRejectsInvalidHubProjectUID(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "!!!!!!!!!!!!!!!!!!!!!!!!!!",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "response: %s", raw)
	assert.Contains(t, string(raw), "hub_project_uid must be a valid UID")
}

func TestFederationReplicaSetupAdoptExistingRejectsArchivedProjectWithHubUID(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProjectWithUID(ctx, "spoke", "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	_, _, err = env.DB.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester"})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode, "response: %s", raw)
	assert.Contains(t, string(raw), "federation_project_collision")
}

func TestFederationReplicaSetupAdoptExistingRejectsHubUIDBoundToDifferentProject(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	bound, err := env.DB.CreateProjectWithUID(ctx, "already-bound", "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            bound.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		PushEnabled:          true,
		Enabled:              true,
	})
	require.NoError(t, err)
	spoke, err := env.DB.CreateProjectWithUID(ctx, "spoke", "01HZNQ7VFPK1XGD8R5MABCD4EA")
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "federation_project_collision")
	assert.Contains(t, string(raw), "already-bound")
	assert.Contains(t, string(raw), "spoke")
	unchanged, err := env.DB.ProjectByID(ctx, spoke.ID)
	require.NoError(t, err)
	assert.Equal(t, "01HZNQ7VFPK1XGD8R5MABCD4EA", unchanged.UID)
	_, err = env.DB.FederationBindingByProject(ctx, spoke.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestFederationReplicaSetupAdoptExistingRequiresPullAndPushCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
	}{
		{name: "missing push", capabilities: "pull"},
		{name: "missing pull", capabilities: "push"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := testenv.New(t)
			ctx := context.Background()
			_, err := env.DB.CreateProjectWithUID(ctx, "spoke", "01HZNQ7VFPK1XGD8R5MABCD4EA")
			require.NoError(t, err)

			resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
				"hub_url":                 "http://127.0.0.1:7373",
				"hub_project_id":          42,
				"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
				"project_name":            "spoke",
				"replay_horizon_event_id": 9,
				"token":                   "push-token",
				"capabilities":            tt.capabilities,
				"push_enabled":            true,
				"adopt_existing":          true,
			}, nil)

			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
			assert.Contains(t, string(raw), "federation_capability_mismatch")
		})
	}
}

func TestFederationReplicaSetupAdoptExistingRejectsMissingLocalProject(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "typo-spoke",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
		"adopt_existing":          true,
	}, nil)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "federation_project_not_found")
	projects, err := env.DB.ListProjects(context.Background())
	require.NoError(t, err)
	assert.Empty(t, projects)
}

func TestFederationReplicaSetupRejectsCredentialDowngradeOnPushBinding(t *testing.T) {
	env := testenv.New(t)
	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
	}
	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &out)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body["token"] = "pull-token"
	body["capabilities"] = "pull"
	body["push_enabled"] = false
	resp = envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	creds, err := config.ReadFederationCredentials()
	require.NoError(t, err)
	assert.Equal(t, "push-token", creds.Projects[out.Project.UID].Token)
	assert.Equal(t, "pull,push", creds.Projects[out.Project.UID].Capabilities)
}

func TestFederationReplicaSetupPushRetryPreservesHigherCursors(t *testing.T) {
	env := testenv.New(t)
	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
		"capabilities":            "pull,push",
		"push_enabled":            true,
	}
	var first api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &first)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, env.DB.AdvanceFederationPullCursor(context.Background(), first.Project.ID, 99))
	require.NoError(t, env.DB.AdvanceFederationPushCursor(context.Background(), first.Project.ID, 0))
	res, err := env.DB.ExecContext(context.Background(), `
		INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		)
		VALUES(
			'01HZNQ7VFPK1XGD8R5MABCD4EW', ?, ?, ?,
			'project.metadata_updated', 'tester', '{}', 1, 0,
			'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
			'2026-05-23T12:00:00.000Z'
		)`,
		env.DB.InstanceUID(), first.Project.ID, first.Project.Name)
	require.NoError(t, err)
	pendingEventID, err := res.LastInsertId()
	require.NoError(t, err)
	require.Greater(t, pendingEventID, int64(0))

	var second api.CreateFederationReplicaBody
	resp = envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &second)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.True(t, second.Binding.PushEnabled)
	assert.Equal(t, int64(99), second.Binding.PullCursorEventID)
	assert.Equal(t, int64(0), second.Binding.PushCursorEventID)
}

func TestFederationReplicaSetupPushDisabledRemainsReadOnly(t *testing.T) {
	env := testenv.New(t)

	var out api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"push_enabled":            false,
	}, &out)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, out.Binding.PushEnabled)

	resp = envDoJSON(t, env, http.MethodPost, projectPath(out.Project.ID)+"/issues", map[string]any{
		"title": "local write",
		"actor": "tester",
	}, nil)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestFederationReplicaSetupCanUpgradePhase1BindingToPush(t *testing.T) {
	env := testenv.New(t)
	body := map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
		"token":                   "push-token",
	}
	var first api.CreateFederationReplicaBody
	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &first)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, first.Binding.PushEnabled)
	res, err := env.DB.ExecContext(context.Background(), `
		INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		)
		VALUES(
			'01HZNQ7VFPK1XGD8R5MABCD4EZ', ?, ?, ?,
			'project.metadata_updated', 'tester', '{}', 1, 0,
			'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			'2026-05-23T12:00:00.000Z'
		)`,
		env.DB.InstanceUID(), first.Project.ID, first.Project.Name)
	require.NoError(t, err)
	localEventID, err := res.LastInsertId()
	require.NoError(t, err)
	body["push_enabled"] = true
	body["capabilities"] = "pull,push"

	var upgraded api.CreateFederationReplicaBody
	resp = envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", body, &upgraded)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	assert.True(t, upgraded.Binding.PushEnabled)
	assert.Equal(t, localEventID, upgraded.Binding.PushCursorEventID)
}

func TestFederationReplicaSetupRejectsUnboundProjectCollision(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	_, err := env.DB.CreateProjectWithUID(ctx, "hub", "01HZNQ7VFPK1XGD8R5MABCD4EX")
	require.NoError(t, err)

	resp := envDoJSON(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "hub",
		"replay_horizon_event_id": 9,
	}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestFederationReplicaSetupValidatesProjectName(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "bad\nname",
		"replay_horizon_event_id": 9,
	}, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(raw), "validation")
}

func TestFederationReplicaSetupRejectsNameCollisionWithDifferentUID(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	_, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url":                 "http://127.0.0.1:7373",
		"hub_project_id":          42,
		"hub_project_uid":         "01HZNQ7VFPK1XGD8R5MABCD4EX",
		"project_name":            "spoke",
		"replay_horizon_event_id": 9,
	}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "federation_project_collision")
	assert.Contains(t, string(raw), "--adopt-existing --push")
}

func TestFederatedSpokeMutationReturnsReadOnlyError(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		Enabled:              true,
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost, projectPath(project.ID)+"/issues", map[string]any{
		"title": "local write",
		"actor": "tester",
	}, nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Contains(t, string(raw), "federated_read_only")
}

func TestFederationAuthConfiguredBearerStillProtectsCRUDAndEnrollmentSetup(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))

	resp, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/projects", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, string(raw), "auth_required")

	resp, _ = envDoRaw(t, env, http.MethodGet, "/api/v1/projects", nil, bearer("admin-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := map[string]any{
		"spoke_instance_uid": federationTestSpokeUID,
		"project_id":         nil,
		"capabilities":       "pull,push",
		"token":              "setup-token",
	}
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", body, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, string(raw), "auth_required")

	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", body, bearer("admin-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin bearer should authorize enrollment setup: %s", raw)
}

func TestFederationStatusNoBindingsIsEmpty(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "local")
	require.NoError(t, err)

	var global federationStatusBodyForTest
	envGetJSON(t, env, "/api/v1/federation/status", &global)
	assert.Empty(t, global.Statuses)

	var scoped federationStatusBodyForTest
	envGetJSON(t, env, projectPath(project.ID)+"/federation/status", &scoped)
	assert.Empty(t, scoped.Statuses)
}

func TestFederationStatusSpokeIncludesCursorsQueuesAndLastSync(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    12,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	issue, localEvent, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "pending local push",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.ExecContext(ctx, `
		INSERT INTO events(
			uid, origin_instance_uid, project_id, project_name,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash
		)
		VALUES(
			'01HZNQ7VFPK1XGD8R5MABCD4PY', ?, ?, ?,
			'project.removed', 'tester', '{}', 1, 0,
			'dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'
		)`,
		env.DB.InstanceUID(), project.ID, project.Name)
	require.NoError(t, err)
	lastPull := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	lastPush := time.Date(2026, 5, 23, 12, 5, 0, 0, time.UTC)
	lastErrorAt := time.Date(2026, 5, 23, 12, 7, 0, 0, time.UTC)
	require.NoError(t, env.DB.RecordFederationSyncPullSuccess(ctx, project.ID, lastPull))
	require.NoError(t, env.DB.RecordFederationSyncPushSuccess(ctx, project.ID, lastPush))
	require.NoError(t, env.DB.RecordFederationSyncError(ctx, project.ID, errors.New("hub offline"), lastErrorAt))
	_, err = env.DB.EnqueuePendingClaim(ctx, db.PendingClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Purpose:   "edit",
		Now:       lastPull,
	})
	require.NoError(t, err)

	var got federationStatusBodyForTest
	envGetJSON(t, env, "/api/v1/federation/status", &got)

	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, project.ID, status.ProjectID)
	assert.Equal(t, project.UID, status.ProjectUID)
	assert.Equal(t, "spoke", status.ProjectName)
	assert.Equal(t, "spoke", status.Role)
	assert.True(t, status.Enabled)
	assert.True(t, status.PushEnabled)
	assert.Equal(t, int64(12), status.PullCursorEventID)
	assert.Equal(t, int64(0), status.PushCursorEventID)
	assert.Equal(t, int64(1), status.PendingPushCount)
	assert.Equal(t, localEvent.ID, status.PendingPushHighWaterEventID)
	assert.Equal(t, int64(1), status.PendingClaimCount)
	assert.Equal(t, int64(0), status.EnrollmentCount)
	assert.Equal(t, int64(0), status.LiveClaimCount)
	assert.Equal(t, int64(0), status.UnresolvedViolationCount)
	assert.Equal(t, int64(0), status.RecentViolationCount)
	require.NotNil(t, status.LastPullSuccessAt)
	assert.True(t, lastPull.Equal(*status.LastPullSuccessAt))
	require.NotNil(t, status.LastPushSuccessAt)
	assert.True(t, lastPush.Equal(*status.LastPushSuccessAt))
	require.NotNil(t, status.LastSuccessfulSyncAt)
	assert.True(t, lastPush.Equal(*status.LastSuccessfulSyncAt))
	require.NotNil(t, status.LastErrorAt)
	assert.True(t, lastErrorAt.Equal(*status.LastErrorAt))
	require.NotNil(t, status.LastError)
	assert.Equal(t, "hub offline", *status.LastError)
	assert.LessOrEqual(t, localEvent.ID, status.PendingPushHighWaterEventID)
}

func TestFederationStatusIncludesActiveQuarantine(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    12,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	createdAt := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	q, err := env.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7", "evt-8", "evt-9"},
		Error:        "hub rejected batch",
		CreatedAt:    createdAt,
	})
	require.NoError(t, err)

	var got federationStatusBodyForTest
	envGetJSON(t, env, "/api/v1/federation/status", &got)

	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, int64(1), status.ActiveQuarantineCount)
	assert.Equal(t, "quarantine", status.ResetBlocker)
	require.Len(t, status.ActiveQuarantines, 1)
	assert.Equal(t, q.ID, status.ActiveQuarantines[0].ID)
	assert.Equal(t, "push", status.ActiveQuarantines[0].Direction)
	assert.Equal(t, int64(7), status.ActiveQuarantines[0].FirstEventID)
	assert.Equal(t, int64(9), status.ActiveQuarantines[0].LastEventID)
	assert.Equal(t, []string{"evt-7", "evt-8", "evt-9"}, status.ActiveQuarantines[0].EventUIDs)
	assert.Equal(t, "hub rejected batch", status.ActiveQuarantines[0].Error)
	assert.True(t, createdAt.Equal(status.ActiveQuarantines[0].CreatedAt))
}

func TestFederationQuarantineSkipRequiresConfirmAndAdvancesCursor(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, q := createPushQuarantineFixture(t, env)
	path := fmt.Sprintf("/api/v1/projects/%d/federation/quarantine/%d/skip", project.ID, q.ID)

	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"actor":  "operator",
		"reason": "intentional skip",
	}, nil)
	require.Equal(t, http.StatusPreconditionFailed, resp.StatusCode, string(raw))
	assert.Contains(t, string(raw), "confirm_required")

	var out api.FederationQuarantineSummary
	resp, raw = envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"actor":  "operator",
		"reason": "intentional skip",
	}, map[string]string{"X-Kata-Confirm": fmt.Sprintf("SKIP FEDERATION BATCH %d", q.ID)})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, q.ID, out.ID)
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(9), binding.PushCursorEventID)
}

func TestFederationQuarantineSkipIdentityModeBootstrapTokenCannotWrite(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	project, q := createPushQuarantineFixture(t, env)
	path := fmt.Sprintf("/api/v1/projects/%d/federation/quarantine/%d/skip", project.ID, q.ID)

	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"actor":  "spoofed",
		"reason": "intentional skip",
	}, map[string]string{
		"Authorization":  "Bearer bootstrap-token",
		"X-Kata-Confirm": fmt.Sprintf("SKIP FEDERATION BATCH %d", q.ID),
	})

	assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "bootstrap_token_write_forbidden")
	active, err := env.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Nil(t, active.SkippedAt)
}

func TestFederationQuarantineSkipIdentityModeUsesDBTokenActor(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	ctx := context.Background()
	project, q := createPushQuarantineFixture(t, env)
	_, _, err := env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "alice-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/federation/quarantine/%d/skip", project.ID, q.ID)

	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"actor":  "spoofed",
		"reason": "intentional skip",
	}, map[string]string{
		"Authorization":  "Bearer alice-token",
		"X-Kata-Confirm": fmt.Sprintf("SKIP FEDERATION BATCH %d", q.ID),
	})

	require.Equal(t, http.StatusOK, resp.StatusCode, "response: %s", raw)
	var skippedBy string
	require.NoError(t, env.DB.QueryRow(`
		SELECT skipped_by
		  FROM federation_quarantine
		 WHERE id = ?`,
		q.ID).Scan(&skippedBy))
	assert.Equal(t, "alice", skippedBy)
}

func TestFederationQuarantineSkipRejectsWrongProjectWithoutMutation(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	other, err := env.DB.CreateProject(ctx, "other")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    12,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	q, err := env.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7"},
		Error:        "hub rejected batch",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)
	path := fmt.Sprintf("/api/v1/projects/%d/federation/quarantine/%d/skip", other.ID, q.ID)

	resp, raw := envDoRaw(t, env, http.MethodPost, path, map[string]any{
		"actor":  "operator",
		"reason": "intentional skip",
	}, map[string]string{"X-Kata-Confirm": fmt.Sprintf("SKIP FEDERATION BATCH %d", q.ID)})

	require.Equal(t, http.StatusNotFound, resp.StatusCode, string(raw))
	active, err := env.DB.ActiveFederationQuarantine(ctx, project.ID, db.FederationQuarantineDirectionPush)
	require.NoError(t, err)
	assert.Nil(t, active.SkippedAt)
	binding, err := env.DB.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), binding.PushCursorEventID)
}

func TestFederationStatusHubIncludesEnrollmentsClaimsAndViolations(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "claimed hub issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "project-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull,push,claim",
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "hard",
		Purpose:   "edit",
		Now:       time.Date(2026, 5, 23, 13, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)

	var got federationStatusBodyForTest
	envGetJSON(t, env, projectPath(project.ID)+"/federation/status", &got)

	require.Len(t, got.Statuses, 1)
	status := got.Statuses[0]
	assert.Equal(t, project.ID, status.ProjectID)
	assert.Equal(t, "hub", status.Role)
	assert.True(t, status.Enabled)
	assert.False(t, status.PushEnabled)
	assert.Equal(t, int64(1), status.EnrollmentCount)
	assert.Equal(t, int64(1), status.LiveClaimCount)
	assert.Equal(t, int64(0), status.PendingPushCount)
	assert.Equal(t, int64(0), status.PendingClaimCount)
	assert.Equal(t, int64(0), status.UnresolvedViolationCount)
	assert.Equal(t, int64(0), status.RecentViolationCount)
}

func TestFederationStatusGlobalSkipsArchivedFederatedProjects(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	active := createFederatedHubProject(t, env, "active-hub")
	archived := createFederatedHubProject(t, env, "archived-hub")
	_, _, err := env.DB.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: archived.ID,
		Actor:     "tester",
	})
	require.NoError(t, err)

	var got federationStatusBodyForTest
	envGetJSON(t, env, "/api/v1/federation/status", &got)

	require.Len(t, got.Statuses, 1)
	assert.Equal(t, active.ID, got.Statuses[0].ProjectID)
	assert.Equal(t, "active-hub", got.Statuses[0].ProjectName)
}

func TestFederationStatusDoesNotCountExpiredTimedClaimAsLive(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "expired timed claim",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, err = env.DB.AcquireClaim(ctx, db.AcquireClaimParams{
		ProjectID: project.ID,
		IssueRef:  issue.ShortID,
		Principal: db.ClaimPrincipal{
			HolderInstanceUID: federationTestSpokeUID,
			Holder:            "agent-a",
			ClientKind:        "cli",
		},
		ClaimKind: "timed",
		TTL:       time.Hour,
		Purpose:   "edit",
		Now:       time.Now().UTC().Add(-2 * time.Hour),
	})
	require.NoError(t, err)

	var got federationStatusBodyForTest
	envGetJSON(t, env, projectPath(project.ID)+"/federation/status", &got)

	require.Len(t, got.Statuses, 1)
	assert.Equal(t, int64(0), got.Statuses[0].LiveClaimCount)
}

func TestFederationStatusUsesAdminBearerNotEnrollmentBearer(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	enrollment, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "enrollment-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet, "/api/v1/federation/status", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "missing bearer response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet, "/api/v1/federation/status", nil, bearer(enrollment.Token))
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "enrollment bearer response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet, "/api/v1/federation/status", nil, bearer("admin-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "admin bearer response: %s", raw)
}

func TestFederationEnrollmentExplicitTokenCreatesRowAndHidesHash(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", map[string]any{
		"spoke_instance_uid": federationTestSpokeUID,
		"project_id":         nil,
		"capabilities":       "push,pull",
		"token":              "explicit-enrollment-token",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "create enrollment response: %s", raw)
	assert.NotContains(t, string(raw), "token_hash")

	var out struct {
		ID               int64  `json:"id"`
		SpokeInstanceUID string `json:"spoke_instance_uid"`
		ProjectID        *int64 `json:"project_id"`
		Capabilities     string `json:"capabilities"`
		Token            string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Positive(t, out.ID)
	assert.Equal(t, federationTestSpokeUID, out.SpokeInstanceUID)
	assert.Nil(t, out.ProjectID)
	assert.Equal(t, "pull,push", out.Capabilities)
	assert.Equal(t, "explicit-enrollment-token", out.Token)

	var (
		tokenHash    string
		projectID    sql.NullInt64
		capabilities string
	)
	require.NoError(t, env.DB.QueryRow(`
		SELECT token_hash, project_id, capabilities
		  FROM federation_enrollments
		 WHERE id = ?`, out.ID).Scan(&tokenHash, &projectID, &capabilities))
	assert.Equal(t, db.FederationTokenHash("explicit-enrollment-token"), tokenHash)
	assert.False(t, projectID.Valid)
	assert.Equal(t, "pull,push", capabilities)
}

func TestFederationEnrollmentOmittedTokenReturnsGeneratedPlaintextOnce(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", map[string]any{
		"spoke_instance_uid": federationTestSpokeUID,
		"project_id":         project.ID,
		"capabilities":       "pull",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "create enrollment response: %s", raw)
	assert.NotContains(t, string(raw), "token_hash")

	var out struct {
		ID      int64  `json:"id"`
		Token   string `json:"token"`
		Project *int64 `json:"project_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.NotEmpty(t, out.Token)
	require.NotNil(t, out.Project)
	assert.Equal(t, project.ID, *out.Project)

	enrollment, err := env.DB.AuthorizeFederationToken(ctx, out.Token, project.ID, "pull")
	require.NoError(t, err)
	assert.Equal(t, federationTestSpokeUID, enrollment.SpokeInstanceUID)

	var tokenHash string
	require.NoError(t, env.DB.QueryRow(`
		SELECT token_hash
		  FROM federation_enrollments
		 WHERE id = ?`, out.ID).Scan(&tokenHash))
	assert.Equal(t, db.FederationTokenHash(out.Token), tokenHash)
	assert.NotEqual(t, out.Token, tokenHash)
}

func TestFederationEnrollmentNullProjectIDCreatesWildcardGrant(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", map[string]any{
		"spoke_instance_uid": federationTestSpokeUID,
		"project_id":         nil,
		"capabilities":       "pull",
		"token":              "wildcard-token",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "create enrollment response: %s", raw)

	var out struct {
		ID        int64  `json:"id"`
		ProjectID *int64 `json:"project_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Nil(t, out.ProjectID)

	var projectID sql.NullInt64
	require.NoError(t, env.DB.QueryRow(`
		SELECT project_id
		  FROM federation_enrollments
		 WHERE id = ?`, out.ID).Scan(&projectID))
	assert.False(t, projectID.Valid)
}

func TestFederationEnrollmentInvalidCapabilitiesReturnsValidation(t *testing.T) {
	env := testenv.New(t)

	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", map[string]any{
		"spoke_instance_uid": federationTestSpokeUID,
		"project_id":         nil,
		"capabilities":       "pull,admin",
		"token":              "bad-capability-token",
	}, nil)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(raw), "validation")
}

func TestFederationAuthTransportPathsRequireEnrollmentBearer(t *testing.T) {
	for _, tc := range federationTransportCases() {
		t.Run(tc.name+"/missing", func(t *testing.T) {
			env := testenv.New(t)
			resp, raw := envDoRaw(t, env, tc.method, tc.path, tc.body, nil)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s response: %s", tc.name, raw)
			assert.Contains(t, string(raw), "auth_required")
		})

		t.Run(tc.name+"/unknown", func(t *testing.T) {
			env := testenv.New(t)
			resp, raw := envDoRaw(t, env, tc.method, tc.path, tc.body, bearer("unknown-federation-token"))
			require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s response: %s", tc.name, raw)
			assert.Contains(t, string(raw), "auth_invalid")
		})

		t.Run(tc.name+"/admin-bearer", func(t *testing.T) {
			env := testenv.New(t, testenv.WithAuthToken("admin-token"))
			resp, raw := envDoRaw(t, env, tc.method, tc.path, tc.body, bearer("admin-token"))
			require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s response: %s", tc.name, raw)
			assert.Contains(t, string(raw), "auth_invalid")
		})
	}
}

func TestFederationAuthValidEnrollmentTokenReachesTransportHandlers(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "transport-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/events?after_id=0", nil, bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "pull response: %s", raw)
	assert.Contains(t, string(raw), `"events"`)

	resp, raw = envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest",
		federationIngestBody(), bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "ingest response: %s", raw)
	var out struct {
		Accepted          int64 `json:"accepted"`
		Duplicates        int64 `json:"duplicates"`
		PushCursorEventID int64 `json:"push_cursor_event_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, int64(0), out.Accepted)
	assert.Equal(t, int64(0), out.Duplicates)
	assert.Equal(t, int64(0), out.PushCursorEventID)
	assert.NotContains(t, string(raw), "spoke_instance_uid")
}

func TestFederationTransportPullMatchesProjectPollBody(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	_, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local hub event",
		Author:    "tester",
	})
	require.NoError(t, err)
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "pull-parity-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	normalResp, normalRaw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/events?after_id=0&limit=100", nil, nil)
	require.Equal(t, http.StatusOK, normalResp.StatusCode, "normal poll response: %s", normalRaw)
	federationResp, federationRaw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/events?after_id=0&limit=100", nil, bearer(created.Token))
	require.Equal(t, http.StatusOK, federationResp.StatusCode, "federation poll response: %s", federationRaw)

	var normal, federated api.PollEventsBody
	require.NoError(t, json.Unmarshal(normalRaw, &normal))
	require.NoError(t, json.Unmarshal(federationRaw, &federated))
	assert.Equal(t, normal, federated)
}

func TestFederationTransportRejectsWrongCapability(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	pullOnly, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "pull-only-transport-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)
	pushOnly, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "push-only-transport-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest",
		federationIngestBody(), bearer(pullOnly.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "pull-only ingest response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/events", nil, bearer(pushOnly.Token))
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "push-only pull response: %s", raw)
}

func TestFederationMetadataTransportUsesEnrollmentPullAuth(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("admin-token"))
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "metadata-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull",
	})
	require.NoError(t, err)

	resp, raw := envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/metadata", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "missing metadata response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/metadata", nil, bearer("admin-token"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "admin metadata response: %s", raw)

	resp, raw = envDoRaw(t, env, http.MethodGet,
		projectPath(project.ID)+"/federation/metadata", nil, bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "enrollment metadata response: %s", raw)
	var out api.ProjectFederationBody
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, project.ID, out.ProjectID)
	assert.Equal(t, project.UID, out.ProjectUID)
}

func TestFederationIngestPersistsBroadcastsAndReturnsAck(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "ingest-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := federationIngestBody(federationIngestEnvelope(t, int64(17), ev))
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	beforeEvents := countEvents(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "ingest response: %s", raw)
	var out struct {
		Accepted          int64 `json:"accepted"`
		Duplicates        int64 `json:"duplicates"`
		PushCursorEventID int64 `json:"push_cursor_event_id"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, int64(1), out.Accepted)
	assert.Equal(t, int64(0), out.Duplicates)
	assert.Equal(t, int64(17), out.PushCursorEventID)
	assert.Equal(t, beforeEvents+1, countEvents(t, env.DB))

	msg := receiveMsg(t, sub.Ch, time.Second, "federation ingest broadcast")
	require.Equal(t, "event", msg.Kind)
	require.NotNil(t, msg.Event)
	assert.Equal(t, project.ID, msg.ProjectID)
	assert.Equal(t, ev.EventUID, msg.Event.UID)
	assert.Equal(t, "issue.created", msg.Event.Type)

	issue, err := env.DB.IssueByShortID(ctx, project.ID, "cd4ec", db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "spoke work", issue.Title)

	resp, raw = envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "duplicate retry response: %s", raw)
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, int64(0), out.Accepted)
	assert.Equal(t, int64(1), out.Duplicates)
	assert.Equal(t, int64(17), out.PushCursorEventID)
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "duplicate ingest should not rebroadcast")
}

func TestFederationIngestRejectsFutureSchemaVersion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "future-schema-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := map[string]any{
		"schema_version": db.CurrentSchemaVersion() + 1,
		"events":         []any{federationIngestEnvelope(t, int64(17), ev)},
	}
	beforeEvents := countEvents(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "future schema response: %s", raw)
	assert.Contains(t, string(raw), "schema_version")
	assert.Equal(t, beforeEvents, countEvents(t, env.DB))
}

func TestFederationIngestRejectsMissingSchemaVersion(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "missing-schema",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := map[string]any{"events": []any{federationIngestEnvelope(t, int64(17), ev)}}
	beforeEvents := countEvents(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "missing schema response: %s", raw)
	assert.Contains(t, string(raw), "schema_version")
	assert.Equal(t, beforeEvents, countEvents(t, env.DB))
}

func TestFederationIngestInvalidBatchReturnsErrorAndDoesNotBroadcast(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "invalid-ingest-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := federationIngestBody(federationIngestEnvelope(t, int64(18), ev))
	body["events"].([]any)[0].(map[string]any)["content_hash"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	beforeEvents := countEvents(t, env.DB)
	beforeIssues := countIssues(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "invalid ingest response: %s", raw)
	assert.Contains(t, string(raw), "validation")
	assert.Equal(t, beforeEvents, countEvents(t, env.DB))
	assert.Equal(t, beforeIssues, countIssues(t, env.DB))
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "invalid ingest should not broadcast")
}

func TestFederationIngestRejectsInvalidSourceCursor(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{ //nolint:gosec // test-only bearer token
		Token:            "invalid-cursor-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "push",
	})
	require.NoError(t, err)
	ev := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := federationIngestBody(federationIngestEnvelope(t, int64(0), ev))
	sub := env.Broadcaster.Subscribe(daemon.SubFilter{ProjectID: project.ID})
	defer sub.Unsub()
	beforeEvents := countEvents(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost,
		projectPath(project.ID)+"/federation/events:ingest", body, bearer(created.Token))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "invalid cursor response: %s", raw)
	assert.Contains(t, string(raw), "validation")
	assert.Equal(t, beforeEvents, countEvents(t, env.DB))
	assertNoReceive(t, sub.Ch, 100*time.Millisecond, "invalid cursor should not broadcast")
}

func TestFederationAuthProjectScopedTokenRejectsNonFederatedProject(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "plain")
	require.NoError(t, err)
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "plain-project-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	beforeEvents := countEvents(t, env.DB)
	beforeIssues := countIssues(t, env.DB)

	for _, tc := range federationTransportCasesForProjectWithIngestBody(project) {
		t.Run(tc.name, func(t *testing.T) {
			resp, raw := envDoRaw(t, env, tc.method, tc.path, tc.body, bearer(created.Token))

			require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s response: %s", tc.name, raw)
			assert.Contains(t, string(raw), "auth_invalid")
			assert.Equal(t, beforeEvents, countEvents(t, env.DB), "%s should not insert events", tc.name)
			assert.Equal(t, beforeIssues, countIssues(t, env.DB), "%s should not fold issues", tc.name)
		})
	}
}

func TestFederationEnrollmentRevokedEnrollmentNoLongerAuthorizesTransport(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	project := createFederatedHubProject(t, env, "hub")
	created, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token:            "revoked-token",
		SpokeInstanceUID: federationTestSpokeUID,
		ProjectID:        &project.ID,
		Capabilities:     "pull,push",
	})
	require.NoError(t, err)
	require.NoError(t, env.DB.RevokeFederationEnrollment(ctx, created.Enrollment.ID))
	beforeEvents := countEvents(t, env.DB)

	for _, tc := range federationTransportCasesForProject(project.ID) {
		resp, raw := envDoRaw(t, env, tc.method, tc.path, tc.body, bearer(created.Token))
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "%s response: %s", tc.name, raw)
		assert.Contains(t, string(raw), "auth_invalid")
	}
	assert.Equal(t, beforeEvents, countEvents(t, env.DB))
}

func assertFederationEventCount(t *testing.T, store *sqlitestore.Store, eventType string, expected int) {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRow(
		`SELECT count(*) FROM events WHERE type = ?`, eventType).Scan(&got))
	assert.Equal(t, expected, got)
}

func createPushQuarantineFixture(t *testing.T, env *testenv.Env) (db.Project, db.FederationQuarantine) {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, "spoke")
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        project.UID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    12,
		PushEnabled:          true,
		PushCursorEventID:    0,
		Enabled:              true,
	})
	require.NoError(t, err)
	q, err := env.DB.RecordFederationQuarantine(ctx, db.RecordFederationQuarantineParams{
		ProjectID:    project.ID,
		Direction:    db.FederationQuarantineDirectionPush,
		FirstEventID: 7,
		LastEventID:  9,
		EventUIDs:    []string{"evt-7"},
		Error:        "hub rejected batch",
		CreatedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)
	return project, q
}

func createFederatedHubProject(t *testing.T, env *testenv.Env, name string) db.Project {
	t.Helper()
	ctx := context.Background()
	project, err := env.DB.CreateProject(ctx, name)
	require.NoError(t, err)
	_, err = env.DB.EnableProjectFederation(ctx, project.ID, "tester")
	require.NoError(t, err)
	return project
}

func countEvents(t *testing.T, store *sqlitestore.Store) int {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRow(`SELECT count(*) FROM events`).Scan(&got))
	return got
}

func countIssues(t *testing.T, store *sqlitestore.Store) int {
	t.Helper()
	var got int
	require.NoError(t, store.QueryRow(`SELECT count(*) FROM issues`).Scan(&got))
	return got
}

func federationRemoteIssueCreatedEvent(t *testing.T, project db.Project, spokeUID string) db.RemoteEvent {
	t.Helper()
	issueUID := "01HZNQ7VFPK1XGD8R5MABCD4EC"
	createdAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	payload := json.RawMessage(`{"uid":"01HZNQ7VFPK1XGD8R5MABCD4EC","short_id":"cd4ec","title":"spoke work","body":"","author":"spoke","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`)
	ev := db.RemoteEvent{
		EventUID:          "01HZNQ7VFPK1XGD8R5MABCD4EB",
		OriginInstanceUID: spokeUID,
		ProjectUID:        project.UID,
		ProjectName:       project.Name,
		IssueUID:          &issueUID,
		Type:              "issue.created",
		Actor:             "spoke",
		HLCPhysicalMS:     1,
		HLCCounter:        0,
		Payload:           payload,
		CreatedAt:         createdAt,
	}
	hash, err := db.EventContentHash(db.EventHashInput{
		UID:               ev.EventUID,
		OriginInstanceUID: ev.OriginInstanceUID,
		ProjectUID:        ev.ProjectUID,
		ProjectName:       ev.ProjectName,
		IssueUID:          ev.IssueUID,
		RelatedIssueUID:   ev.RelatedIssueUID,
		Type:              ev.Type,
		Actor:             ev.Actor,
		HLCPhysicalMS:     ev.HLCPhysicalMS,
		HLCCounter:        ev.HLCCounter,
		CreatedAt:         ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Payload:           ev.Payload,
	})
	require.NoError(t, err)
	ev.ContentHash = hash
	return ev
}

func federationIngestEnvelope(t *testing.T, sourceEventID int64, ev db.RemoteEvent) map[string]any {
	t.Helper()
	var payload any
	require.NoError(t, json.Unmarshal(ev.Payload, &payload))
	out := map[string]any{
		"event_id":            sourceEventID,
		"event_uid":           ev.EventUID,
		"origin_instance_uid": ev.OriginInstanceUID,
		"project_uid":         ev.ProjectUID,
		"project_name":        ev.ProjectName,
		"type":                ev.Type,
		"actor":               ev.Actor,
		"hlc_physical_ms":     ev.HLCPhysicalMS,
		"hlc_counter":         ev.HLCCounter,
		"content_hash":        ev.ContentHash,
		"payload":             payload,
		"created_at":          ev.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if ev.IssueUID != nil {
		out["issue_uid"] = *ev.IssueUID
	}
	if ev.RelatedIssueUID != nil {
		out["related_issue_uid"] = *ev.RelatedIssueUID
	}
	return out
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

type federationStatusBodyForTest struct {
	Statuses []struct {
		ProjectID                   int64  `json:"project_id"`
		ProjectUID                  string `json:"project_uid"`
		ProjectName                 string `json:"project_name"`
		Role                        string `json:"role"`
		Enabled                     bool   `json:"enabled"`
		PushEnabled                 bool   `json:"push_enabled"`
		PullCursorEventID           int64  `json:"pull_cursor_event_id"`
		PushCursorEventID           int64  `json:"push_cursor_event_id"`
		PendingPushCount            int64  `json:"pending_push_count"`
		PendingPushHighWaterEventID int64  `json:"pending_push_high_water_event_id"`
		EnrollmentCount             int64  `json:"enrollment_count"`
		LiveClaimCount              int64  `json:"live_claim_count"`
		PendingClaimCount           int64  `json:"pending_claim_count"`
		ActiveQuarantineCount       int64  `json:"active_quarantine_count"`
		ResetBlocker                string `json:"reset_blocker,omitempty"`
		ActiveQuarantines           []struct {
			ID           int64     `json:"id"`
			Direction    string    `json:"direction"`
			FirstEventID int64     `json:"first_event_id"`
			LastEventID  int64     `json:"last_event_id"`
			EventUIDs    []string  `json:"event_uids"`
			Error        string    `json:"error"`
			CreatedAt    time.Time `json:"created_at"`
		} `json:"active_quarantines"`
		UnresolvedViolationCount int64      `json:"unresolved_violation_count"`
		RecentViolationCount     int64      `json:"recent_violation_count"`
		LastSuccessfulSyncAt     *time.Time `json:"last_successful_sync_at,omitempty"`
		LastPullSuccessAt        *time.Time `json:"last_pull_success_at,omitempty"`
		LastPushSuccessAt        *time.Time `json:"last_push_success_at,omitempty"`
		LastErrorAt              *time.Time `json:"last_error_at,omitempty"`
		LastError                *string    `json:"last_error,omitempty"`
	} `json:"statuses"`
}

type federationTransportCase struct {
	name   string
	method string
	path   string
	body   any
}

func federationTransportCases() []federationTransportCase {
	return federationTransportCasesForProject(1)
}

func federationTransportCasesForProject(projectID int64) []federationTransportCase {
	base := projectPath(projectID)
	return []federationTransportCase{
		{
			name:   "pull",
			method: http.MethodGet,
			path:   base + "/federation/events",
		},
		{
			name:   "push",
			method: http.MethodPost,
			path:   base + "/federation/events:ingest",
			body:   federationIngestBody(),
		},
	}
}

func federationTransportCasesForProjectWithIngestBody(project db.Project) []federationTransportCase {
	cases := federationTransportCasesForProject(project.ID)
	cases[1].body = map[string]any{
		"schema_version": db.CurrentSchemaVersion(),
		"events": []map[string]any{{
			"event_id":            1,
			"event_uid":           "01HZNQ7VFPK1XGD8R5MABCD4EB",
			"origin_instance_uid": federationTestSpokeUID,
			"type":                "issue.snapshot",
			"project_uid":         project.UID,
			"project_name":        project.Name,
			"issue_uid":           "01HZNQ7VFPK1XGD8R5MABCD4EC",
			"actor":               "remote-agent",
			"hlc_physical_ms":     1,
			"hlc_counter":         0,
			"content_hash":        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"payload": map[string]any{
				"uid":        "01HZNQ7VFPK1XGD8R5MABCD4EC",
				"short_id":   "ABCD4EC",
				"title":      "remote",
				"body":       "",
				"author":     "remote-agent",
				"status":     "open",
				"metadata":   map[string]any{},
				"created_at": "2026-05-23T12:00:00.000Z",
			},
			"created_at": "2026-05-23T12:00:00.000Z",
		}},
	}
	return cases
}

func federationIngestBody(events ...any) map[string]any {
	if events == nil {
		events = []any{}
	}
	return map[string]any{
		"schema_version": db.CurrentSchemaVersion(),
		"events":         events,
	}
}
