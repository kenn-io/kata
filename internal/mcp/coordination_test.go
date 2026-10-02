package mcpserver

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/notification"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestNewSectionLoadersRegisterOnlyTheirTools(t *testing.T) {
	for _, test := range []struct {
		loader, section string
		tools           []string
	}{
		{"kata.load_coordination", "coordination", []string{"kata.assign", "kata.inbox", "kata.status", "kata.unassign"}},
		{"kata.load_docs", "docs", []string{"kata.read_doc", "kata.search_docs"}},
	} {
		t.Run(test.section, func(t *testing.T) {
			session := connectRawTestServerWithOptions(t, Options{
				Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project",
				Actor: "example-agent", Version: "test-version",
			})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: test.loader, Arguments: map[string]any{}})
			require.NoError(t, err)
			require.False(t, result.IsError)
			var output ToolSectionOutput
			require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
			require.Equal(t, ToolSectionOutput{Section: test.section, Available: true, Loaded: true, Tools: test.tools}, output)

			listed, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			want := append(append([]string(nil), sectionLoaderNames...), test.tools...)
			sort.Strings(want)
			require.Equal(t, want, toolNames(listed.Tools))
		})
	}
}

func newCoordinationSession(t *testing.T) (*sdkmcp.ClientSession, *sqlitestore.Store) {
	t.Helper()
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	daemonServer := daemon.NewServer(daemon.ServerConfig{DB: store, StartedAt: time.Now().UTC()})
	t.Cleanup(func() { require.NoError(t, daemonServer.Close()) })
	httpServer := httptest.NewServer(daemonServer.Handler())
	t.Cleanup(httpServer.Close)
	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, ProjectID: project.ID, ProjectName: project.Name,
		Actor: "example-agent", Version: "test-version",
	})
	return session, store
}

func callToolError(t *testing.T, session *sdkmcp.ClientSession, name string, arguments map[string]any) {
	t.Helper()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: arguments})
	if err == nil {
		require.True(t, result.IsError, "%s: %s", name, mustJSON(t, result))
	}
}

func TestAssignStatusAndUnassignRoundTripAgainstDaemon(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Hand off review", "idempotency_key": "handoff-1"})
	ref := created["issue"].(map[string]any)["ref"].(string)

	status := callAdministrationTool(t, session, "kata.status", map[string]any{"ref": ref})
	require.Equal(t, "unassigned", status["hold"])
	require.Equal(t, "example-agent", status["actor"])
	require.Equal(t, "startup", status["actor_source"])
	require.Equal(t, "spoke-project#"+ref, status["qualified_ref"])

	assigned := callAdministrationTool(t, session, "kata.assign", map[string]any{"ref": ref, "owner": "reviewer-agent"})
	require.Equal(t, true, assigned["changed"])
	require.Equal(t, "reviewer-agent", assigned["issue"].(map[string]any)["owner"])
	status = callAdministrationTool(t, session, "kata.status", map[string]any{"ref": ref})
	require.Equal(t, "assigned", status["hold"])
	require.Equal(t, "reviewer-agent", status["owner"])

	callToolError(t, session, "kata.assign", map[string]any{"ref": ref, "owner": "   "})
	callToolError(t, session, "kata.unassign", map[string]any{"ref": ref, "expected_owner": "someone-else"})
	status = callAdministrationTool(t, session, "kata.status", map[string]any{"ref": ref})
	require.Equal(t, "reviewer-agent", status["owner"], "a failed guard must keep the owner")

	cleared := callAdministrationTool(t, session, "kata.unassign", map[string]any{"ref": ref, "expected_owner": "reviewer-agent"})
	require.Equal(t, true, cleared["changed"])
	require.NotContains(t, cleared["issue"].(map[string]any), "owner")
	status = callAdministrationTool(t, session, "kata.status", map[string]any{"ref": ref})
	require.Equal(t, "unassigned", status["hold"])

	callToolError(t, session, "kata.status", map[string]any{"ref": "other-project#abcd"})
}

func TestInboxReadsAttentionRequestsAgainstDaemon(t *testing.T) {
	session, _ := newCoordinationSession(t)
	key := notification.MetadataKey("reviewer-agent")
	requests := []map[string]any{
		{"from": "example-agent", "message": "Please review the plan"},
		{"from": "example-agent", "teammate": "planner", "message": "Second look"},
		{"from": "", "message": "Malformed request"},
	}
	for i, value := range requests {
		created := callAdministrationTool(t, session, "kata.create", map[string]any{
			"title": "Attention issue", "idempotency_key": "attention-" + string(rune('a'+i)),
		})
		ref := created["issue"].(map[string]any)["ref"].(string)
		callAdministrationTool(t, session, "kata.set_metadata", map[string]any{"ref": ref, "patch": map[string]any{key: value}})
	}
	callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Quiet issue", "idempotency_key": "quiet"})

	inbox := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": " reviewer-agent "})
	require.Equal(t, "reviewer-agent", inbox["recipient"])
	require.Equal(t, false, inbox["truncated"])
	listed := inbox["requests"].([]any)
	require.Len(t, listed, 2, "malformed and unrelated issues must not appear")
	messages := map[string]map[string]any{}
	for _, item := range listed {
		request := item.(map[string]any)
		messages[request["message"].(string)] = request
		require.Equal(t, "example-agent", request["from"])
		require.NotEmpty(t, request["qualified_ref"])
	}
	require.Equal(t, "planner", messages["Second look"]["teammate"])
	require.NotContains(t, messages["Please review the plan"], "teammate")

	limited := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer-agent", "limit": 1})
	require.Equal(t, true, limited["truncated"])
	require.LessOrEqual(t, len(limited["requests"].([]any)), 1)

	again := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer-agent"})
	require.Len(t, again["requests"], 2, "reading must not clear requests")
	empty := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "nobody"})
	require.Empty(t, empty["requests"])
	callToolError(t, session, "kata.inbox", map[string]any{"for": "   "})
}

func TestProjectShowReturnsProjectAndAliasesAgainstDaemon(t *testing.T) {
	session, store := newCoordinationSession(t)
	project, err := store.ProjectByName(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = store.AttachAlias(t.Context(), project.ID, "host-a.example/repository", "git")
	require.NoError(t, err)

	shown := callAdministrationTool(t, session, "kata.project_show", map[string]any{})
	require.Equal(t, "spoke-project", shown["project"].(map[string]any)["name"])
	aliases := shown["aliases"].([]any)
	require.Len(t, aliases, 1)
	require.Equal(t, "host-a.example/repository", aliases[0].(map[string]any)["identity"])
	require.Equal(t, "git", aliases[0].(map[string]any)["kind"])

	callToolError(t, session, "kata.project_show", map[string]any{"project": "other-project"})
}
