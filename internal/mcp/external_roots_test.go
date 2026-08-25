package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	kataclient "go.kenn.io/kata/pkg/client"
)

type externalRootRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

var externalRootToolNames = []string{
	"kata.bridge_bind",
	"kata.bridge_pause",
	"kata.bridge_reconcile",
	"kata.bridge_resolve_comment",
	"kata.bridge_resolve_field",
	"kata.bridge_resume",
	"kata.bridge_show",
	"kata.bridge_unbind",
	"kata.connector_field_map",
	"kata.connector_field_unmap",
	"kata.connector_fields",
	"kata.connectors",
}

func TestExternalRootSectionPublishesTypedTools(t *testing.T) {
	session := connectRawTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, ProjectID: 42, ProjectName: "spoke-project",
		Actor: "example-agent", Version: "test-version",
	})

	for range 2 {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
			Name: "kata.load_external_roots", Arguments: map[string]any{},
		})
		require.NoError(t, err)
		require.False(t, result.IsError)
		var output ToolSectionOutput
		require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &output))
		require.Equal(t, ToolSectionOutput{
			Section: "external_roots", Available: true, Loaded: true,
			Tools: externalRootToolNames,
		}, output)
	}

	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	want := append(append([]string(nil), sectionLoaderNames...), externalRootToolNames...)
	require.ElementsMatch(t, want, toolNames(result.Tools))
}

func TestExternalRootInputSchemasBoundEnumsAndCommentModes(t *testing.T) {
	session := connectTestServerWithOptions(t, Options{
		Client: &kataclient.Client{}, Scope: NewAllScope(), Actor: "example-agent", Version: "test-version",
	})
	result, err := session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	byName := make(map[string]*sdkmcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		byName[tool.Name] = tool
	}

	mapProperties := schemaObject(t, byName["kata.connector_field_map"].InputSchema)["properties"].(map[string]any)
	require.ElementsMatch(t, []any{"scheduled_on", "deadline_on"}, mapProperties["kata_field"].(map[string]any)["enum"])
	require.EqualValues(t, 256, mapProperties["external_field"].(map[string]any)["maxLength"])
	resolveProperties := schemaObject(t, byName["kata.bridge_resolve_field"].InputSchema)["properties"].(map[string]any)
	require.ElementsMatch(t, []any{"kata", "external"}, resolveProperties["use"].(map[string]any)["enum"])
	require.Len(t, schemaObject(t, byName["kata.bridge_resolve_comment"].InputSchema)["allOf"], 1)

	for _, arguments := range []map[string]any{
		{"ref": "spoke-project#abc4", "action": "adopt"},
		{"ref": "spoke-project#abc4", "action": "retry", "external_comment_id": "unexpected"},
	} {
		called, callErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
			Name: "kata.bridge_resolve_comment", Arguments: arguments,
		})
		require.NoError(t, callErr)
		require.True(t, called.IsError)
		require.Contains(t, string(mustJSON(t, called)), "validating")
	}
}

func TestConnectorToolsUseTypedDaemonRoutes(t *testing.T) {
	requests := make([]externalRootRequest, 0, 5)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			if len(data) != 0 {
				require.NoError(t, json.Unmarshal(data, &body))
			}
		}
		requests = append(requests, externalRootRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/connectors":
			writeExternalRootJSON(t, w, map[string]any{"connectors": []any{externalConnectorFixture()}})
		case "GET /api/v1/connectors/example-connector":
			writeExternalRootJSON(t, w, externalConnectorFixture())
		case "GET /api/v1/connectors/example-connector/fields":
			writeExternalRootJSON(t, w, map[string]any{"fields": []any{map[string]any{
				"id": "start-date", "display_name": "Start date", "accepted_kinds": []string{"date", "local_datetime"},
				"nullable": true, "writable": true, "schema_revision": "schema-v1",
			}}})
		case "PUT /api/v1/connectors/example-connector/fields/scheduled_on":
			writeExternalRootJSON(t, w, externalMappingFixture(true))
		case "DELETE /api/v1/connectors/example-connector/fields/scheduled_on":
			writeExternalRootJSON(t, w, externalMappingFixture(false))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(httpServer.Close)

	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: NewAllScope(), Actor: "example-agent", Version: "test-version",
	})

	listed := callWorkflowTool(t, session, "kata.connectors", map[string]any{})
	require.Len(t, listed["connectors"], 1)
	status := callWorkflowTool(t, session, "kata.connectors", map[string]any{"instance": " example-connector "})
	require.Len(t, status["connectors"], 1)
	fields := callWorkflowTool(t, session, "kata.connector_fields", map[string]any{"instance": "example-connector"})
	require.Equal(t, "example-connector", fields["instance"])
	require.Len(t, fields["fields"], 1)
	mapped := callWorkflowTool(t, session, "kata.connector_field_map", map[string]any{
		"instance": "example-connector", "kata_field": "scheduled_on", "external_field": "start-date",
	})
	require.Equal(t, "example-connector", mapped["instance"])
	unmapped := callWorkflowTool(t, session, "kata.connector_field_unmap", map[string]any{
		"instance": "example-connector", "kata_field": "scheduled_on",
	})
	require.Equal(t, "example-connector", unmapped["instance"])

	require.Equal(t, []externalRootRequest{
		{Method: http.MethodGet, Path: "/api/v1/connectors"},
		{Method: http.MethodGet, Path: "/api/v1/connectors/example-connector"},
		{Method: http.MethodGet, Path: "/api/v1/connectors/example-connector/fields"},
		{Method: http.MethodPut, Path: "/api/v1/connectors/example-connector/fields/scheduled_on", Body: map[string]any{"external_field": "start-date"}},
		{Method: http.MethodDelete, Path: "/api/v1/connectors/example-connector/fields/scheduled_on"},
	}, requests)
}

func TestBridgeToolsUseScopedIssueRoutesAndServerActor(t *testing.T) {
	requests := make([]externalRootRequest, 0, 8)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			if len(data) != 0 {
				require.NoError(t, json.Unmarshal(data, &body))
			}
		}
		requests = append(requests, externalRootRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects/42/issues/abc4/bridge/actions/reconcile" {
			writeExternalRootJSON(t, w, externalRunFixture())
			return
		}
		if r.URL.Path == "/api/v1/projects/42/issues/abc4/bridge" ||
			strings.HasPrefix(r.URL.Path, "/api/v1/projects/42/issues/abc4/bridge/actions/") {
			writeExternalRootJSON(t, w, externalBridgeFixture())
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(httpServer.Close)

	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, LongRunningClient: client, Scope: scope, Actor: "example-agent", Version: "test-version",
	})

	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{name: "kata.bridge_show", args: map[string]any{"ref": "abc4"}},
		{name: "kata.bridge_reconcile", args: map[string]any{"ref": "abc4"}},
		{name: "kata.bridge_pause", args: map[string]any{"ref": "abc4", "reason": "operator pause"}},
		{name: "kata.bridge_resume", args: map[string]any{"ref": "abc4"}},
		{name: "kata.bridge_resolve_field", args: map[string]any{"ref": "abc4", "kata_field": "scheduled_on", "use": "external"}},
		{name: "kata.bridge_resolve_comment", args: map[string]any{"ref": "abc4", "action": "adopt", "external_comment_id": "opaque ID/with spaces:%25"}},
		{name: "kata.bridge_unbind", args: map[string]any{"ref": "abc4"}},
	} {
		output := callWorkflowTool(t, session, call.name, call.args)
		require.Equal(t, "spoke-project", output["project"].(map[string]any)["name"])
	}

	path := "/api/v1/projects/42/issues/abc4/bridge"
	require.Equal(t, []externalRootRequest{
		{Method: http.MethodGet, Path: path},
		{Method: http.MethodPost, Path: path + "/actions/reconcile", Body: map[string]any{"actor": "example-agent"}},
		{Method: http.MethodPost, Path: path + "/actions/pause", Body: map[string]any{"actor": "example-agent", "reason": "operator pause"}},
		{Method: http.MethodPost, Path: path + "/actions/resume", Body: map[string]any{"actor": "example-agent"}},
		{Method: http.MethodPost, Path: path + "/actions/resolve-field", Body: map[string]any{"actor": "example-agent", "kata_field": "scheduled_on", "use": "external"}},
		{Method: http.MethodPost, Path: path + "/actions/resolve-comment", Body: map[string]any{"action": "adopt", "actor": "example-agent", "external_comment_id": "opaque ID/with spaces:%25"}},
		{Method: http.MethodDelete, Path: path, Query: "actor=example-agent"},
	}, requests)
}

func TestBridgeUnbindResolvesArchivedProjectWithinStartupScope(t *testing.T) {
	requests := make([]externalRootRequest, 0, 2)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, externalRootRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/projects":
			require.Equal(t, "archived", r.URL.Query().Get("include"))
			archived := projectJSON(42, "01HAAAAAAAAAAAAAAAAAAAAAAA", "archived-project")
			archived["deleted_at"] = "2026-08-22T12:00:00.000Z"
			writeExternalRootJSON(t, w, map[string]any{"projects": []any{archived}})
		case "DELETE /api/v1/projects/42/issues/abc4/bridge":
			writeExternalRootJSON(t, w, externalBridgeFixture())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(httpServer.Close)

	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	scope, err := NewBoundScope(ProjectIdentity{
		ID: 42, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "archived-project",
	})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{
		Client: client, Scope: scope, Actor: "example-agent", Version: "test-version",
	})

	output := callWorkflowTool(t, session, "kata.bridge_unbind", map[string]any{"ref": "abc4"})
	require.Equal(t, "archived-project", output["project"].(map[string]any)["name"])
	require.Equal(t, []externalRootRequest{
		{Method: http.MethodGet, Path: "/api/v1/projects", Query: "include=archived"},
		{Method: http.MethodDelete, Path: "/api/v1/projects/42/issues/abc4/bridge", Query: "actor=example-agent"},
	}, requests)
}

func TestExternalRootMutationsKeepStartupScope(t *testing.T) {
	bound, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	boundHandlers := toolHandlers{options: Options{Client: &kataclient.Client{}, Scope: bound, Actor: "example-agent"}}

	_, _, err = boundHandlers.connectorFieldMap(t.Context(), nil, ConnectorFieldMapInput{
		Instance: "example-connector", KataField: "scheduled_on", ExternalField: "start-date",
	})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	_, _, err = boundHandlers.connectorFieldUnmap(t.Context(), nil, ConnectorFieldUnmapInput{
		Instance: "example-connector", KataField: "scheduled_on",
	})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")

	allowlist, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 42, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)
	allowlistHandlers := toolHandlers{options: Options{Client: &kataclient.Client{}, Scope: allowlist, Actor: "example-agent"}}
	_, _, err = allowlistHandlers.bridgePause(t.Context(), nil, BridgePauseInput{Ref: "abc4"})
	require.ErrorContains(t, err, "multi-project writes require project#ref")
}

func TestConnectorMetadataRequiresAllProjectsScopeBeforeDaemonRequest(t *testing.T) {
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeExternalRootJSON(t, w, map[string]any{"connectors": []any{externalConnectorFixture()}})
	}))
	t.Cleanup(httpServer.Close)

	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	allowlist, err := NewAllowlistScope([]ProjectIdentity{{
		ID: 42, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "spoke-project",
	}})
	require.NoError(t, err)

	for name, scope := range map[string]*Scope{"bound": bound, "allowlist": allowlist} {
		t.Run(name, func(t *testing.T) {
			handlers := toolHandlers{options: Options{Client: client, LongRunningClient: client, Scope: scope}}
			_, _, err := handlers.connectors(t.Context(), nil, ConnectorsInput{})
			require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
			_, _, err = handlers.connectorFields(t.Context(), nil, ConnectorFieldsInput{Instance: "example-connector"})
			require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
		})
	}
	require.Zero(t, requests.Load(), "scoped connector metadata reads must fail before any daemon request")
}

func TestBridgeBindRequiresAllProjectsScopeBeforeUsingConnectorCredentials(t *testing.T) {
	var requests atomic.Int64
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		writeExternalRootJSON(t, w, externalBridgeFixture())
	}))
	t.Cleanup(httpServer.Close)

	client, err := kataclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	bound, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{Client: client, LongRunningClient: client, Scope: bound, Actor: "example-agent"}}

	_, _, err = handlers.bridgeBind(t.Context(), nil, BridgeBindInput{
		Ref: "abc4", Connector: "example-connector", External: "root-locator",
	})
	require.ErrorContains(t, err, "requires the --all-projects daemon-wide scope")
	require.Zero(t, requests.Load())
}

func TestExternalConnectorCallsUseLongRunningClient(t *testing.T) {
	var defaultRequests atomic.Int64
	defaultServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects" {
			writeExternalRootJSON(t, w, map[string]any{"projects": []any{
				projectJSON(42, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project"),
			}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/connectors/") {
			writeExternalRootJSON(t, w, externalMappingFixture(false))
			return
		}
		writeExternalRootJSON(t, w, externalBridgeFixture())
	}))
	t.Cleanup(defaultServer.Close)
	defaultClient, err := kataclient.NewWithHTTPClient(defaultServer.URL, defaultServer.Client())
	require.NoError(t, err)

	var longRequests atomic.Int64
	longServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		longRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/connectors":
			writeExternalRootJSON(t, w, map[string]any{"connectors": []any{externalConnectorFixture()}})
		case r.URL.Path == "/api/v1/connectors/example-connector":
			writeExternalRootJSON(t, w, externalConnectorFixture())
		case r.URL.Path == "/api/v1/connectors/example-connector/fields" && r.Method == http.MethodGet:
			writeExternalRootJSON(t, w, map[string]any{"fields": []any{}})
		case r.URL.Path == "/api/v1/connectors/example-connector/fields/scheduled_on":
			writeExternalRootJSON(t, w, externalMappingFixture(true))
		case strings.HasSuffix(r.URL.Path, "/actions/reconcile"):
			writeExternalRootJSON(t, w, externalRunFixture())
		default:
			writeExternalRootJSON(t, w, externalBridgeFixture())
		}
	}))
	t.Cleanup(longServer.Close)
	longClient, err := kataclient.NewWithHTTPClient(longServer.URL, longServer.Client())
	require.NoError(t, err)

	scope, err := NewBoundScope(ProjectIdentity{ID: 42, Name: "spoke-project"})
	require.NoError(t, err)
	handlers := toolHandlers{options: Options{
		Client: defaultClient, LongRunningClient: longClient, Scope: scope, Actor: "example-agent",
	}}
	wideHandlers := handlers
	wideHandlers.options.Scope = NewAllScope()
	_, _, err = wideHandlers.connectors(t.Context(), nil, ConnectorsInput{})
	require.NoError(t, err)
	_, _, err = wideHandlers.connectors(t.Context(), nil, ConnectorsInput{Instance: "example-connector"})
	require.NoError(t, err)
	_, _, err = wideHandlers.connectorFields(t.Context(), nil, ConnectorFieldsInput{Instance: "example-connector"})
	require.NoError(t, err)
	_, _, err = wideHandlers.connectorFieldMap(t.Context(), nil, ConnectorFieldMapInput{
		Instance: "example-connector", KataField: "scheduled_on", ExternalField: "start-date",
	})
	require.NoError(t, err)
	_, _, err = wideHandlers.bridgeBind(t.Context(), nil, BridgeBindInput{
		Ref: "spoke-project#abc4", Connector: "example-connector", External: "root-locator",
	})
	require.NoError(t, err)
	_, _, err = handlers.bridgeReconcile(t.Context(), nil, BridgeInput{Ref: "abc4"})
	require.NoError(t, err)
	_, _, err = handlers.bridgeResume(t.Context(), nil, BridgeInput{Ref: "abc4"})
	require.NoError(t, err)
	_, _, err = handlers.bridgeResolveField(t.Context(), nil, BridgeResolveFieldInput{
		Ref: "abc4", KataField: "scheduled_on", Use: "external",
	})
	require.NoError(t, err)
	_, _, err = handlers.bridgeResolveComment(t.Context(), nil, BridgeResolveCommentInput{
		Ref: "abc4", Action: "retry",
	})
	require.NoError(t, err)
	_, _, err = wideHandlers.connectorFieldUnmap(t.Context(), nil, ConnectorFieldUnmapInput{
		Instance: "example-connector", KataField: "scheduled_on",
	})
	require.NoError(t, err)
	_, _, err = handlers.bridgeShow(t.Context(), nil, BridgeInput{Ref: "abc4"})
	require.NoError(t, err)
	_, _, err = handlers.bridgePause(t.Context(), nil, BridgePauseInput{Ref: "abc4"})
	require.NoError(t, err)
	_, _, err = handlers.bridgeUnbind(t.Context(), nil, BridgeInput{Ref: "abc4"})
	require.NoError(t, err)
	require.EqualValues(t, 4, defaultRequests.Load())
	require.EqualValues(t, 10, longRequests.Load())
}

func writeExternalRootJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(value))
}

func externalConnectorFixture() map[string]any {
	return map[string]any{
		"instance_id": "example-connector", "connector_id": "generic.connector", "display_name": "Generic connector",
		"protocol": "kata.connector/v1", "capabilities": []string{"fields"}, "healthy": true,
	}
}

func externalMappingFixture(active bool) map[string]any {
	return map[string]any{
		"kata_field": "scheduled_on", "external_field_id": "start-date", "external_field_name": "Start date",
		"accepted_kinds": []string{"date", "local_datetime"}, "nullable": true, "writable": true,
		"schema_revision": "schema-v1", "active": active,
	}
}

func externalBridgeFixture() map[string]any {
	return map[string]any{
		"id": 7, "uid": "01JBRIDGE00000000000000000", "project_id": 42, "issue_id": 9,
		"connector_instance": "example-connector", "active": true, "enabled": true,
		"receive_comments": true, "publish_comments": false, "complete_external": true,
		"consecutive_failures": 0,
	}
}

func externalRunFixture() map[string]any {
	return map[string]any{
		"bridge": externalBridgeFixture(), "root_updated": true, "comments_created": 1,
		"comments_edited": 0, "field_conflicts": 0, "completion_requests": 0, "reopen_requests": 0, "paused": false,
	}
}
