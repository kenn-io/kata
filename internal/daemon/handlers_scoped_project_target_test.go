package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

// TestIssueScopedTokenCannotAttachAliasThroughProjectResolution pins the
// scoped project-resolution boundary: alias-bearing project selectors are an
// unsupported operation for issue-scoped credentials and must be rejected
// before any domain resolution, so a rejected request cannot leave a
// first-seen alias attached to an out-of-scope project.
func TestIssueScopedTokenCannotAttachAliasThroughProjectResolution(t *testing.T) {
	env := testenv.New(t, testenv.WithAuthToken("bootstrap-token"), testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	otherProject, err := env.DB.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	root := createScopedHTTPTestIssue(t, env, project.ID, "Root", nil)

	expiresAt := time.Now().UTC().Add(time.Hour)
	token, _, err := env.DB.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "worker-token", Actor: "worker-a", AdminActor: db.BootstrapActor,
		Scope: &db.APITokenScope{
			Kind: db.APITokenScopeIssueSubtree, ProjectUID: project.UID, RootIssueUID: root.UID,
		},
		ExpiresAt: &expiresAt,
	})
	require.NoError(t, err)

	cfg := daemon.ServerConfig{DB: env.DB}
	server := daemon.NewServer(cfg)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := daemon.WithPrincipal(r.Context(), daemon.Principal{
			Kind: daemon.PrincipalDBToken, Subject: token.Actor, Actor: token.Actor,
			TokenID: token.ID, Scope: token.Scope, ExpiresAt: token.ExpiresAt,
		})
		server.Handler().ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(ts.Close)

	aliasIdentity := "https://daemon.example/example-workspace.git"
	resp := postWithHeader(t, ts, "/api/v1/projects/name:other-project/issues", map[string]string{
		"X-Kata-Project-Alias":      aliasIdentity,
		"X-Kata-Project-Alias-Kind": "git",
	}, map[string]string{"actor": "worker-a", "title": "never created"})
	require.Equal(t, http.StatusForbidden, resp.status, string(resp.body))

	_, err = env.DB.AliasByIdentity(t.Context(), aliasIdentity)
	require.ErrorIs(t, err, db.ErrNotFound,
		"a rejected scoped request must not leave an alias attached")
	aliases, err := env.DB.ProjectAliases(t.Context(), otherProject.ID)
	require.NoError(t, err)
	require.Empty(t, aliases)

	// The grant itself still resolves its own project through the same
	// wrapper without an alias.
	numeric := "/api/v1/projects/" + nameSelector("example-project") + "/issues"
	resp = postWithHeader(t, ts, numeric, nil, map[string]any{
		"actor": "worker-a", "title": "in-scope child",
		"links": []map[string]any{{"type": "parent", "to_ref": root.ShortID}},
	})
	require.Equal(t, http.StatusOK, resp.status, string(resp.body))
}

func nameSelector(name string) string { return "name:" + name }
