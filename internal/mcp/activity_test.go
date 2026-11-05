package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestNextPriorityAgainstDaemon(t *testing.T) {
	bound, err := NewBoundScope(ProjectIdentity{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"})
	require.NoError(t, err)
	allowlist, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "hub-project"},
	})
	require.NoError(t, err)
	for _, scope := range []*Scope{bound, allowlist, NewAllScope()} {
		t.Run(string(scope.Mode()), func(t *testing.T) {
			var healthCalls, readyCalls, catalogCalls atomic.Int32
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					healthCalls.Add(1)
					writeJSON(w, map[string]any{"ok": true, "api_schema_version": "0.41.0"})
				case "/api/v1/projects":
					catalogCalls.Add(1)
					writeJSON(w, map[string]any{"projects": []any{
						projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
						projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project"),
					}})
				case "/api/v1/projects/1/ready", "/api/v1/projects/2/ready", "/api/v1/ready":
					readyCalls.Add(1)
					assert.Equal(t, "1", r.URL.Query().Get("limit"))
					assert.Equal(t, "priority", r.URL.Query().Get("sort"))
					issue := globalIssueJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "hub-project", "best")
					issue["priority"] = 1
					issue["title"] = "priority pick"
					if r.URL.Path == "/api/v1/projects/1/ready" {
						assert.Equal(t, ScopeAllowlist, scope.Mode())
						issue = issueJSON(1, "spoke-project", "lower")
						issue["priority"] = 2
						issue["updated_at"] = "2026-08-12T00:00:00Z"
					} else if scope.Mode() == ScopeAll {
						assert.Equal(t, "/api/v1/ready", r.URL.Path)
					} else {
						assert.Equal(t, "/api/v1/projects/2/ready", r.URL.Path)
					}
					writeJSON(w, map[string]any{"issues": []any{issue}})
				default:
					http.NotFound(w, r)
				}
			})
			handlers := toolHandlers{options: Options{Client: client, Scope: scope}}
			_, output, err := handlers.next(t.Context(), nil, NextInput{})
			require.NoError(t, err)
			require.NotNil(t, output.Issue)
			require.Equal(t, "hub-project#best", output.Issue.QualifiedRef)
			require.Equal(t, int32(1), healthCalls.Load())
			wantReady := int32(1)
			if scope.Mode() == ScopeAllowlist {
				wantReady = 2
			}
			require.Equal(t, wantReady, readyCalls.Load())
			if scope.Mode() == ScopeAll {
				require.Zero(t, catalogCalls.Load())
			}
		})
	}
}

func TestWorkflowToolsArePublished(t *testing.T) {
	session := connectTestServer(t)
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	for _, name := range []string{
		"kata.audit_closes", "kata.delete", "kata.edit_comment", "kata.graph",
		"kata.move", "kata.next", "kata.purge", "kata.restore", "kata.wait",
	} {
		require.True(t, names[name], name)
	}
}

func TestWorkflowToolsRoundTripAgainstDaemon(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	spoke, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	hub, err := store.CreateProject(t.Context(), "hub-project")
	require.NoError(t, err)
	daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	httpServer := httptest.NewServer(daemonServer.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: spoke.ID, UID: spoke.UID, Name: spoke.Name},
		{ID: hub.ID, UID: hub.UID, Name: hub.Name},
	})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "example-agent", Version: "test-version"})

	created := callWorkflowTool(t, session, "kata.create", map[string]any{
		"project": "spoke-project", "title": "Workflow issue", "priority": 1, "idempotency_key": "workflow-issue",
	})
	qualified := created["issue"].(map[string]any)["qualified_ref"].(string)
	next := callWorkflowTool(t, session, "kata.next", map[string]any{"project": "spoke-project"})
	require.Equal(t, qualified, next["issue"].(map[string]any)["qualified_ref"])

	commented := callWorkflowTool(t, session, "kata.comment", map[string]any{
		"ref": qualified, "body": "Initial note", "idempotency_key": "workflow-note",
	})
	commentUID := commented["comment"].(map[string]any)["uid"].(string)
	edited := callWorkflowTool(t, session, "kata.edit_comment", map[string]any{
		"ref": qualified, "comment_ref": commentUID, "body": "Updated note",
	})
	require.Equal(t, "Updated note", edited["comment"].(map[string]any)["body"])
	graph := callWorkflowTool(t, session, "kata.graph", map[string]any{"ref": qualified, "depth": "1"})
	require.NotEmpty(t, graph["nodes"])

	moved := callWorkflowTool(t, session, "kata.move", map[string]any{"ref": qualified, "to_project": "hub-project"})
	movedRef := moved["issue"].(map[string]any)["qualified_ref"].(string)
	require.Contains(t, movedRef, "hub-project#")
	callWorkflowTool(t, session, "kata.delete", map[string]any{"ref": movedRef, "confirm": "DELETE " + movedRef})
	callWorkflowTool(t, session, "kata.restore", map[string]any{"ref": movedRef})
	callWorkflowTool(t, session, "kata.delete", map[string]any{"ref": movedRef, "confirm": "DELETE " + movedRef})
	redeleted := callWorkflowTool(t, session, "kata.delete", map[string]any{"ref": movedRef, "confirm": "DELETE " + movedRef})
	require.Equal(t, false, redeleted["changed"], "retrying a committed delete must reach the daemon's idempotent no-op")
	purged := callWorkflowTool(t, session, "kata.purge", map[string]any{"ref": movedRef, "confirm": "PURGE " + movedRef})
	require.Equal(t, true, purged["purged"])

	completed := callWorkflowTool(t, session, "kata.create", map[string]any{
		"project": "spoke-project", "title": "Completed workflow", "idempotency_key": "completed-workflow",
	})
	completedRef := completed["issue"].(map[string]any)["qualified_ref"].(string)
	callWorkflowTool(t, session, "kata.close", map[string]any{
		"ref": completedRef, "reason": "done", "message": "Completed the workflow behavior with daemon evidence.",
		"evidence": []any{map[string]any{"type": "test", "command": "go test ./internal/mcp"}},
	})
	waited := callWorkflowTool(t, session, "kata.wait", map[string]any{"refs": []any{completedRef}, "status": "closed", "timeout_seconds": 1})
	require.Equal(t, "matched", waited["reason"])
	audit := callWorkflowTool(t, session, "kata.audit_closes", map[string]any{"project": "spoke-project"})
	require.NotEmpty(t, audit["rows"])
}

func callWorkflowTool(t *testing.T, session *sdkmcp.ClientSession, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", mustJSON(t, result))
	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok, "%T", result.StructuredContent)
	return structured
}
