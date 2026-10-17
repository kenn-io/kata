package daemon_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestFederationAdoptionPreservesRequiredFormerProjectIdentity(t *testing.T) {
	env := testenv.New(t)
	ctx := t.Context()
	source, target, issue := seedMovePair(t, env)
	for _, project := range []db.Project{source, target} {
		_, err := env.DB.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
	}
	_, err := env.DB.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = env.DB.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: source.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: source.ID, HubProjectUID: source.UID, Actor: "worker", Enabled: true})
	require.NoError(t, err)
	_, err = env.DB.LeaveFederationReplica(ctx, source.ID)
	require.NoError(t, err)
	newUID, err := uid.New()
	require.NoError(t, err)
	resp, raw := envDoRaw(t, env, http.MethodPost, "/api/v1/federation/replicas", map[string]any{
		"hub_url": "http://127.0.0.1:7373", "hub_project_id": 42,
		"hub_project_uid": newUID, "project_name": source.Name,
		"replay_horizon_event_id": 1,
		"actor":                   "worker", "token": "push-token", "capabilities": "pull,push",
		"push_enabled": true, "adopt_existing": true,
	}, nil)
	assertAPIError(t, resp.StatusCode, raw, http.StatusConflict, "federation_project_identity_required")
	require.NotContains(t, string(raw), target.UID)
	require.NotContains(t, string(raw), issue.UID)
	retained, err := env.DB.ProjectByID(ctx, source.ID)
	require.NoError(t, err)
	require.Equal(t, source.UID, retained.UID)
	_, err = env.DB.FederationBindingByProject(ctx, source.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
}
