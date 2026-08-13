package mcpserver

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestAdministrationToolsArePublished(t *testing.T) {
	session := connectTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(), EnableTokenAdmin: true,
		Actor: "example-agent", Version: "test-version",
	})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	require.False(t, names["kata.federation_enroll"], "credential-minting enrollment must stay outside MCP")
	for _, name := range []string{
		"kata.digest", "kata.events", "kata.import_issues",
		"kata.federation_enrollment_revoke",
		"kata.federation_leave", "kata.federation_quarantine", "kata.federation_rebind", "kata.federation_status",
		"kata.project_create", "kata.project_merge", "kata.project_purge", "kata.project_remove",
		"kata.project_restore", "kata.project_update", "kata.projects", "kata.system",
		"kata.recurrence_delete", "kata.recurrence_update", "kata.recurrences",
		"kata.sync_once", "kata.sync_status", "kata.sync_update",
		"kata.token_create", "kata.token_revoke", "kata.tokens",
	} {
		require.True(t, names[name], name)
	}
}

func TestAdministrationToolsRoundTripAgainstDaemon(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.CreateProject(t.Context(), "initial-project")
	require.NoError(t, err)
	daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	httpServer := httptest.NewServer(daemonServer.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: NewAllScope(), EnableTokenAdmin: true, Actor: "example-agent", Version: "test-version"})

	created := callAdministrationTool(t, session, "kata.project_create", map[string]any{"name": "created-project"})
	require.Equal(t, "created-project", created["project"].(map[string]any)["name"])
	updated := callAdministrationTool(t, session, "kata.project_update", map[string]any{"project": "created-project", "action": "rename", "name": "renamed-project"})
	require.Equal(t, "renamed-project", updated["project"].(map[string]any)["name"])

	recurrence := callAdministrationTool(t, session, "kata.recurrence_update", map[string]any{
		"project": "renamed-project", "action": "create", "dtstart": "2026-09-01", "rrule": "FREQ=WEEKLY", "timezone": "UTC",
		"template": map[string]any{"title": "Weekly review", "owner": "example-agent"},
	})["recurrence"].(map[string]any)
	uid := recurrence["uid"].(string)
	require.Equal(t, "example-agent", recurrence["template_owner"])
	cleared := callAdministrationTool(t, session, "kata.recurrence_update", map[string]any{
		"project": "renamed-project", "action": "patch", "uid": uid, "revision": recurrence["revision"],
		"template_patch": map[string]any{"clear_owner": true},
	})["recurrence"].(map[string]any)
	require.NotContains(t, cleared, "template_owner", "template_patch clear_owner must reach the daemon")
	listed := callAdministrationTool(t, session, "kata.recurrences", map[string]any{"project": "renamed-project"})
	require.Len(t, listed["recurrences"], 1)
	callAdministrationTool(t, session, "kata.recurrence_delete", map[string]any{"project": "renamed-project", "uid": uid, "revision": cleared["revision"]})

	now := time.Now().UTC().Format(time.RFC3339)
	imported := callAdministrationTool(t, session, "kata.import_issues", map[string]any{
		"project": "renamed-project", "source": "example-source", "items": []any{map[string]any{
			"external_id": "example-1", "title": "Imported task", "author": "example-agent", "status": "open", "created_at": now, "updated_at": now,
		}},
	})
	require.EqualValues(t, 1, imported["result"].(map[string]any)["created"])

	digest := callAdministrationTool(t, session, "kata.digest", map[string]any{"project": "renamed-project", "since": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)})
	require.NotEmpty(t, digest["digests"])
	events := callAdministrationTool(t, session, "kata.events", map[string]any{"project": "renamed-project", "mode": "poll", "after": 0, "limit": 100})
	require.NotEmpty(t, events["events"])
	system := callAdministrationTool(t, session, "kata.system", map[string]any{})
	require.NotEmpty(t, system["instance_uid"])
	require.NotContains(t, system, "db_path")
	// Enrollment credentials are minted through the CLI/daemon, never MCP.
	renamed, err := store.ProjectByName(t.Context(), "renamed-project")
	require.NoError(t, err)
	actor := "example-agent"
	_, err = client.EnableProjectFederation(t.Context(), &generated.EnableProjectFederationRequestOptions{
		PathParams: &generated.EnableProjectFederationPath{ProjectID: renamed.ID},
		Body:       &generated.EnableProjectFederationBody{Actor: &actor},
	})
	require.NoError(t, err)
	enrolled, err := client.CreateFederationEnrollment(t.Context(), &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{
		Actor: &actor, ProjectID: renamed.ID,
		SpokeInstanceUID: system["instance_uid"].(string), Capabilities: "pull",
	}})
	require.NoError(t, err)
	enrollmentID := enrolled.ID
	federation := callAdministrationTool(t, session, "kata.federation_status", map[string]any{})
	require.NotEmpty(t, federation["enrollments"])
	require.NotContains(t, federation["enrollments"].([]any)[0].(map[string]any), "token")
	callAdministrationTool(t, session, "kata.federation_enrollment_revoke", map[string]any{"id": enrollmentID})
	token := callAdministrationTool(t, session, "kata.token_create", map[string]any{"token_actor": "automation-agent", "name": "automation"})
	require.NotEmpty(t, token["token"])
	tokenID := token["record"].(map[string]any)["id"]
	tokens := callAdministrationTool(t, session, "kata.tokens", map[string]any{})
	require.NotEmpty(t, tokens["tokens"])
	require.NotContains(t, tokens["tokens"].([]any)[0].(map[string]any), "token")
	callAdministrationTool(t, session, "kata.token_revoke", map[string]any{"id": tokenID})
	callAdministrationTool(t, session, "kata.project_create", map[string]any{"name": "lifecycle-project"})
	callAdministrationTool(t, session, "kata.project_remove", map[string]any{"project": "lifecycle-project"})
	callAdministrationTool(t, session, "kata.project_restore", map[string]any{"project": "lifecycle-project"})
	callAdministrationTool(t, session, "kata.project_remove", map[string]any{"project": "lifecycle-project"})
	purged := callAdministrationTool(t, session, "kata.project_purge", map[string]any{"project": "lifecycle-project", "confirm": "PURGE lifecycle-project"})
	require.Equal(t, true, purged["purged"])
}

func callAdministrationTool(t *testing.T, session *sdkmcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s: %s", name, mustJSON(t, result))
	return result.StructuredContent.(map[string]any)
}
