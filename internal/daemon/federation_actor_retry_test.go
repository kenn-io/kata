package daemon_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// Missing old-actor history needs its own owner-issued transport grant. The
// current grant can acknowledge it only after admission under that old actor.
func TestFederationIngestActorRecoveryUsesSeparateGrant(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("owner-test-token"), testenv.WithRequireTokenIdentity())
	ctx := t.Context()
	project := createFederatedHubProject(t, env, "hub-project")
	_, _, err := env.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "historical-account-test-token", Actor: "tester", AdminActor: "bootstrap",
	})
	require.NoError(t, err)
	current, err := env.DB.CreateFederationEnrollment(ctx, db.CreateFederationEnrollmentParams{
		Token: "current-test-token", SpokeInstanceUID: federationTestSpokeUID,
		ProjectID: &project.ID, Capabilities: "push", Actor: "current-worker",
	})
	require.NoError(t, err)
	historical := federationRemoteIssueCreatedEvent(t, project, federationTestSpokeUID)
	body := federationIngestBody(federationIngestEnvelope(t, 17, historical))
	path := projectPath(project.ID) + "/federation/events:ingest"
	beforeCount := countEvents(t, env.DB)

	resp, raw := envDoRaw(t, env, http.MethodPost, path, body, bearer(current.Token))
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
	require.Equal(t, beforeCount, countEvents(t, env.DB), "a current grant cannot admit fresh old-actor history")

	// Create a separate scoped grant, without rotating or replacing the
	// current enrollment or the spoke's production credential.
	resp, raw = envDoRaw(t, env, http.MethodPost, "/api/v1/federation/enrollments", map[string]any{
		"token": "recovery-test-token", "spoke_instance_uid": federationTestSpokeUID,
		"project_id": project.ID, "capabilities": "push", "actor": historical.Actor,
	}, bearer("owner-test-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var recovery struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(raw, &recovery))
	resp, raw = envDoRaw(t, env, http.MethodPost, path, body, bearer("recovery-test-token"))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var admitted api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &admitted))
	require.EqualValues(t, 1, admitted.Accepted)
	stored, err := env.DB.EventsByUIDs(ctx, project.ID, []string{historical.EventUID})
	require.NoError(t, err)

	// Revoking the temporary grant does not revoke the unchanged current one.
	err = env.DB.RevokeFederationEnrollment(ctx, recovery.ID)
	require.NoError(t, err)
	resp, raw = envDoRaw(t, env, http.MethodPost, path, body, bearer("recovery-test-token"))
	assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "auth_invalid")
	resp, raw = envDoRaw(t, env, http.MethodPost, path, body, bearer(current.Token))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", raw)
	var acknowledged api.FederationIngestEventsBody
	require.NoError(t, json.Unmarshal(raw, &acknowledged))
	require.Zero(t, acknowledged.Accepted)
	require.EqualValues(t, 1, acknowledged.Duplicates)
	require.EqualValues(t, 17, acknowledged.PushCursorEventID)
	require.Equal(t, beforeCount+1, countEvents(t, env.DB))
	retained, err := env.DB.EventsByUIDs(ctx, project.ID, []string{historical.EventUID})
	require.NoError(t, err)
	require.Equal(t, stored, retained, "acknowledgement must retain the original event bytes and attribution")
}
