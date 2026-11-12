package daemon_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestIssueScopedProjectLookupHidesMetadata(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	_, err = env.DB.PatchProjectMetadata(t.Context(), db.PatchProjectMetadataIn{
		ProjectID: project.ID, Actor: "owner", Patch: map[string]jsontext.Value{"private": jsontext.Value(`"hidden-value"`)},
	})
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	newScopedTokens(t, env, project, root)
	headers := map[string]string{"Authorization": "Bearer worker-token"}

	ownerResp, ownerBody := envDoRaw(t, env, http.MethodGet, "/api/v1/projects/resolve?name=example-project", nil, map[string]string{"Authorization": "Bearer bootstrap-token"})
	require.Equal(t, http.StatusOK, ownerResp.StatusCode, string(ownerBody))
	require.Contains(t, string(ownerBody), "hidden-value")
	showResp, showBody := envDoRaw(t, env, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d", project.ID), nil, headers)
	require.Equal(t, http.StatusOK, showResp.StatusCode, string(showBody))
	require.NotContains(t, string(showBody), "hidden-value")
	lookupResp, lookupBody := envDoRaw(t, env, http.MethodGet, "/api/v1/projects/resolve?name=example-project", nil, headers)
	if lookupResp.StatusCode == http.StatusOK {
		var show, lookup struct {
			Project map[string]any `json:"project"`
		}
		require.NoError(t, json.Unmarshal(showBody, &show))
		require.NoError(t, json.Unmarshal(lookupBody, &lookup))
		require.Equal(t, show.Project, lookup.Project)
	} else {
		require.Equal(t, http.StatusNotFound, lookupResp.StatusCode, string(lookupBody))
	}
}
