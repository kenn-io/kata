package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

// TestIssueScopedExactUIDLookupCannotProbeHiddenExistence pins the scoped
// direct-lookup contract: a full valid UID for a hidden (subtree-external)
// issue and for a nonexistent issue must return the identical not-found
// envelope, so an exact UID cannot distinguish "exists but hidden" from
// "does not exist". Prefix resolution already narrows to the allowed set.
func TestIssueScopedExactUIDLookupCannotProbeHiddenExistence(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	child := createScopedHTTPTestIssue(t, env, project.ID, "Child", &root)
	hidden := createScopedHTTPTestIssue(t, env, project.ID, "Hidden sibling", nil)

	missingUID, err := uid.New()
	require.NoError(t, err)

	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	// Sanity: an in-subtree UID still resolves for the scoped caller.
	resp, body := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/"+child.UID, nil, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "body: %s", body)

	hiddenResp, hiddenBody := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/"+hidden.UID, nil, headers)
	missingResp, missingBody := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/"+missingUID, nil, headers)

	require.Equal(t, http.StatusNotFound, hiddenResp.StatusCode, string(hiddenBody))
	var hiddenEnvelope api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(hiddenBody, &hiddenEnvelope))
	var missingEnvelope api.ErrorEnvelope
	require.Equal(t, http.StatusNotFound, missingResp.StatusCode, string(missingBody))
	require.NoError(t, json.Unmarshal(missingBody, &missingEnvelope))

	// Both responses share one generic envelope shape; each echoes only the
	// UID the caller itself supplied.
	require.Equal(t, missingEnvelope.Status, hiddenEnvelope.Status)
	require.Equal(t, missingEnvelope.Error.Code, hiddenEnvelope.Error.Code)
	require.Equal(t, missingEnvelope.Error.Hint, hiddenEnvelope.Error.Hint)
	require.Equal(t, missingEnvelope.Error.Data, hiddenEnvelope.Error.Data)
	require.Equal(t, "no issue matches uid "+hidden.UID, hiddenEnvelope.Error.Message)
	require.Equal(t, "no issue matches uid "+missingUID, missingEnvelope.Error.Message)

	// Unscoped callers keep their descriptive miss message.
	unscopedResp, unscopedBody := envDoRaw(t, env, http.MethodGet,
		"/api/v1/issues/"+missingUID, nil,
		map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equal(t, http.StatusNotFound, unscopedResp.StatusCode)
	require.Contains(t, string(unscopedBody), "no issue matches uid "+missingUID)
}

// This companion check allows the response to echo the UID the caller
// supplied, but it must not leak the hidden issue's project identity.
func TestIssueScopedExactUIDLookupHidesProjectIdentity(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	foreign := createScopedHTTPTestIssue(t, env, otherProject.ID, "Foreign issue", nil)
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)

	expiresAt := time.Now().UTC().Add(time.Hour)
	_, _, err = env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	missingUID, err := uid.New()
	require.NoError(t, err)
	foreignResp, foreignBody := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/"+foreign.UID, nil, headers)
	missingResp, missingBody := envDoRaw(t, env, http.MethodGet, "/api/v1/issues/"+missingUID, nil, headers)

	require.Equal(t, http.StatusNotFound, foreignResp.StatusCode)
	require.Equal(t, http.StatusNotFound, missingResp.StatusCode)
	foreignBodyText := string(foreignBody)
	require.NotContains(t, foreignBodyText, "other-project")
	var foreignEnvelope api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(foreignBody, &foreignEnvelope))
	var missingEnvelope api.ErrorEnvelope
	require.NoError(t, json.Unmarshal(missingBody, &missingEnvelope))
	require.Equal(t, missingEnvelope.Status, foreignEnvelope.Status)
	require.Equal(t, missingEnvelope.Error.Code, foreignEnvelope.Error.Code)
	require.Equal(t, "no issue matches uid "+foreign.UID, foreignEnvelope.Error.Message)
	require.Equal(t, "no issue matches uid "+missingUID, missingEnvelope.Error.Message)
}
