package mcpserver

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestScopedTokenToolWritesProtectedFileWithoutReturningPlaintext(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root, _, err := store.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Delegated work", Author: "coordinator",
	})
	require.NoError(t, err)
	daemonServer := daemon.NewServer(daemon.ServerConfig{
		DB: store, StartedAt: time.Now().UTC(),
		Auth: config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true},
	})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	httpServer := httptest.NewServer(daemonServer.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithBearer(t.Context(), httpServer.URL, "bootstrap-token")
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: NewAllScope(), EnableTokenAdmin: true,
		Actor: "coordinator", Version: "test-version",
	})
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod(dir, 0o700)) //nolint:gosec // Directory mode, not a secret file.
	}
	tokenPath := filepath.Join(dir, "worker.token")

	output := callAdministrationTool(t, session, "kata.token_create", map[string]any{
		"token_actor":        "worker-a",
		"name":               "remote-worktree",
		"issue":              project.Name + "#" + root.ShortID,
		"expires_in_seconds": 3600,
		"token_file":         tokenPath,
	})

	require.NotContains(t, output, "token")
	require.Equal(t, testenv.PhysicalPath(t, tokenPath), output["token_file"])
	require.Equal(t, project.Name+"#"+root.ShortID, output["issue"])
	record := output["record"].(map[string]any)
	require.Equal(t, "worker-a", record["actor"])
	scope := record["scope"].(map[string]any)
	require.Equal(t, "issue_subtree", scope["kind"])
	require.Equal(t, project.UID, scope["project_uid"])
	require.Equal(t, root.UID, scope["root_issue_uid"])
	require.NotEmpty(t, record["expires_at"])

	secret, err := os.ReadFile(tokenPath) //nolint:gosec // tokenPath is a test-owned temporary path.
	require.NoError(t, err)
	plaintext := strings.TrimSpace(string(secret))
	require.True(t, strings.HasPrefix(plaintext, "kata_"))
	encoded := string(mustJSON(t, output))
	require.NotContains(t, encoded, plaintext)
	resolved, err := store.ResolveAPIToken(t.Context(), plaintext)
	require.NoError(t, err)
	require.Equal(t, "worker-a", resolved.Actor)
}
