package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestProjectAccessMCPScopeRefresh(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var store db.Storage
			if backend == "sqlite" {
				s, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
				require.NoError(t, err)
				store = s
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
				t.Cleanup(cleanup)
				s, err := pgstore.Open(t.Context(), dsn)
				require.NoError(t, err)
				store = s
			}
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			private, err := store.CreateProject(t.Context(), "restricted-project")
			require.NoError(t, err)
			public, err := store.CreateProject(t.Context(), "shared-project")
			require.NoError(t, err)
			_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: private.ID, Title: "mcp-private-canary", Author: "member"})
			require.NoError(t, err)
			_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: public.ID, Title: "Visible task", Author: "member"})
			require.NoError(t, err)
			team, _, err := store.CreateTeam(t.Context(), "example-team", "admin")
			require.NoError(t, err)
			_, err = store.SetTeamMembership(t.Context(), team.UID, "member", true, "admin")
			require.NoError(t, err)
			_, _, err = store.SetProjectAccessPolicy(t.Context(), db.ProjectAccessPolicy{ProjectUID: private.UID, Visibility: "teams", TeamUIDs: []string{team.UID}}, "admin")
			require.NoError(t, err)
			_, _, err = store.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "member-test-token", Actor: "member", AdminActor: "admin"})
			require.NoError(t, err)
			server := daemon.NewServer(daemon.ServerConfig{DB: store, Auth: config.AuthConfig{RequireTokenIdentity: true}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client(), kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
				r.Header.Set("Authorization", "Bearer member-test-token")
				return nil
			}))
			require.NoError(t, err)
			session := connectTestServerWithOptions(t, Options{Client: client, Scope: NewAllScope(), Actor: "member", Version: "test"})
			call := func(name string) string {
				t.Helper()
				result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
				require.NoError(t, err)
				require.False(t, result.IsError, string(mustJSON(t, result)))
				return string(mustJSON(t, result))
			}
			require.Contains(t, call("kata.projects"), private.UID)
			require.Contains(t, call("kata.list"), "mcp-private-canary")
			_, err = store.SetTeamMembership(t.Context(), team.UID, "member", false, "admin")
			require.NoError(t, err)
			require.NotContains(t, call("kata.projects"), private.UID)
			list := call("kata.list")
			require.NotContains(t, list, "mcp-private-canary")
			require.Contains(t, list, "Visible task")
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{"project": private.Name}})
			require.NoError(t, err)
			require.True(t, result.IsError)
			require.NotContains(t, string(mustJSON(t, result)), private.UID)
		})
	}
}
