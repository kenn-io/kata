package daemon_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestScopedProjectResolveIgnoresAliasWithExplicitName(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree,
			ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	alias := "https://daemon.example/example-project.git"
	resp, body := envDoRaw(t, env, http.MethodPost, "/api/v1/projects/resolve",
		map[string]any{"name": project.Name, "alias": map[string]string{"identity": alias, "kind": "git"}},
		map[string]string{"Authorization": "Bearer worker-token"})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, string(body), project.UID)
	_, err = env.DB.AliasByIdentity(t.Context(), alias)
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestScopedLinkDeletionHidesOutsideParent(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	outside := createScopedHTTPTestIssue(t, env, project.ID, "Outside", nil)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", &outside)
	link, err := env.DB.ParentOf(t.Context(), root.ID)
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{Kind: db.APITokenScopeIssueSubtree,
			ProjectUID: project.UID, RootIssueUID: root.UID},
		ExpiresAt: new(time.Now().UTC().Add(time.Hour)),
	})
	require.NoError(t, err)
	resp, body := envDoRaw(t, env, http.MethodDelete,
		"/api/v1/projects/"+strconv.FormatInt(project.ID, 10)+"/issues/"+root.ShortID+
			"/links/"+strconv.FormatInt(link.ID, 10)+"?actor=worker", nil,
		map[string]string{"Authorization": "Bearer worker-token"})
	assertAPIError(t, resp.StatusCode, body, http.StatusNotFound, "issue_not_found")
}
