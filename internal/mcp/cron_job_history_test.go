package mcpserver

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestCronJobHistoryPagingAndScope(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	args := map[string]any{"uid": id, "name": "Original", "definition": cronDefinition()}
	created := cronRecord(t, f.session, "cron_job_create", args, "job")
	args["name"], args["expected_event_uid"] = "Updated", created["definition_event_uid"]
	updated := cronRecord(t, f.session, "cron_job_update", args, "job")
	deleted := cronRecord(t, f.session, "cron_job_delete", map[string]any{"uid": id, "expected_event_uid": updated["definition_event_uid"]}, "job")

	page := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": " " + strings.ToLower(id) + " ", "project": f.project.Name, "limit": 1})
	versions := page["versions"].([]any)
	require.Len(t, versions, 1)
	latest := versions[0].(map[string]any)
	require.Equal(t, deleted["definition_event_uid"], latest["event_uid"])
	require.Equal(t, latest["event_uid"], page["next_before_event_uid"])

	older := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": id, "limit": 100, "before_event_uid": strings.ToLower(page["next_before_event_uid"].(string))})
	versions = older["versions"].([]any)
	require.Len(t, versions, 2)
	for i, want := range []map[string]any{updated, created} {
		version := versions[i].(map[string]any)
		require.Equal(t, want["definition_event_uid"], version["event_uid"])
		require.Equal(t, want["name"], version["name"])
	}
	require.NotContains(t, older, "next_before_event_uid")
	all := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": id})
	require.Len(t, all["versions"], 3)

	other, err := f.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	allowed := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "example-agent", Version: "test"})
	foreign := cronCall(t, allowed, "cron_job_history", map[string]any{"uid": id, "project": other.Name})
	require.True(t, foreign.IsError)
	require.Contains(t, foreign.Content[0].(*sdkmcp.TextContent).Text, "not_found")
	require.Len(t, cronOK(t, allowed, "cron_job_history", map[string]any{"uid": id, "project": f.project.Name})["versions"], 3)
}

func TestCronJobHistoryErrors(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	cronRecord(t, f.session, "cron_job_create", map[string]any{"uid": id, "name": "Fixture", "definition": cronDefinition()}, "job")
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"bad uid", map[string]any{"uid": "short"}, "ULID"},
		{"zero limit", map[string]any{"uid": id, "limit": 0}, "limit"},
		{"bad cursor", map[string]any{"uid": id, "before_event_uid": "short"}, "ULID"},
		{"unknown job", map[string]any{"uid": cronNewUID(t, f.session)}, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := cronCall(t, f.session, "cron_job_history", tc.args)
			require.True(t, result.IsError)
			require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, tc.want)
		})
	}
}

// A wire fixture exercises metadata that ordinary local edits do not produce:
// baselines, skipped-event diagnostics, and arbitrary opaque numeric literals.
func TestCronJobHistoryPreservesResponse(t *testing.T) {
	definition := cronDefinition()
	definition["options"] = json.RawMessage(`{"integer":9007199254740993,"decimal":1.0000000000000000001}`)
	raw := fmt.Sprintf(`{"versions":[{"actor":null,"current":false,"definition":%s,"definition_hlc":{"counter":2,"origin_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","physical_ms":1794268800000,"version":1},"deleted_at":"2026-11-10T00:00:00Z","event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","name":"Baseline","operation":"baseline","updated_at":"2026-11-10T00:00:00Z"}],"next_before_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","skipped_event_uids":["01ARZ3NDEKTSV4RRFFQ69G5FAX"]}`, mustJSON(t, definition))
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, raw)
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	result := cronCall(t, session, "cron_job_history", map[string]any{"uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	require.False(t, result.IsError)
	require.Len(t, requests, 1)
	request := <-requests
	require.Equal(t, http.MethodGet, request.Method)
	require.Equal(t, "/api/v1/projects/42/cron/jobs/01ARZ3NDEKTSV4RRFFQ69G5FAV/versions", request.URL.Path)
	require.NotContains(t, request.URL.Query(), "limit")
	text := result.Content[0].(*sdkmcp.TextContent).Text
	var want, got any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&want))
	decoder = json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&got))
	require.Equal(t, want, got)
}

func TestCronJobHistoryDiff(t *testing.T) {
	f := newCronFixture(t)
	id := cronNewUID(t, f.session)
	definition := cronDefinition()
	definition["options"] = json.RawMessage(`{"integer":9007199254740993,"lexical":1.0}`)
	args := map[string]any{"uid": id, "name": "Old", "definition": definition}
	created := cronRecord(t, f.session, "cron_job_create", args, "job")
	args["expected_event_uid"] = created["definition_event_uid"]
	unchanged := cronRecord(t, f.session, "cron_job_update", args, "job")
	args["expected_event_uid"], args["name"] = unchanged["definition_event_uid"], "New"
	definition["options"] = json.RawMessage(`{"integer":9007199254740994,"lexical":1e0,"nullable":null}`)
	cronRecord(t, f.session, "cron_job_update", args, "job")
	// Observe history JSON before the SDK client decodes numbers into float64.
	server, err := New(Options{Client: f.client, ProjectID: f.project.ID, ProjectName: f.project.Name, Actor: "example-agent", Version: "test"})
	require.NoError(t, err)
	structured := make(chan string, 1)
	server.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, request sdkmcp.Request) (sdkmcp.Result, error) {
			result, err := next(ctx, method, request)
			if callRequest, ok := request.(*sdkmcp.CallToolRequest); !ok || callRequest.Params.Name != "kata.cron_job_history" {
				return result, err
			}
			if call, ok := result.(*sdkmcp.CallToolResult); ok {
				if raw, ok := call.StructuredContent.(jsontext.Value); ok {
					structured <- string(raw)
				}
			}
			return result, err
		}
	})
	st, ct := sdkmcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ss.Close()) })
	session, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	cronOK(t, session, "load_cron", map[string]any{})
	result := cronCall(t, session, "cron_job_history", map[string]any{"uid": id, "diff": true})
	require.False(t, result.IsError, "%s", result.Content[0].(*sdkmcp.TextContent).Text)
	text := result.Content[0].(*sdkmcp.TextContent).Text
	require.Equal(t, text, <-structured)
	var page struct {
		Versions []struct {
			Changes json.RawMessage `json:"changes"`
		} `json:"versions"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &page))
	require.Len(t, page.Versions, 3)
	require.Contains(t, string(page.Versions[0].Changes), `"before":9007199254740993`)
	require.Contains(t, string(page.Versions[0].Changes), `"after":9007199254740994`)
	require.Contains(t, string(page.Versions[0].Changes), `"before":1.0`)
	require.Contains(t, string(page.Versions[0].Changes), `"after":1e0`)
	require.Equal(t, "[]", string(page.Versions[1].Changes))
	require.Equal(t, "null", string(page.Versions[2].Changes))
	tools, err := f.session.ListTools(t.Context(), nil)
	require.NoError(t, err)
	var historyTool *sdkmcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "kata.cron_job_history" {
			historyTool = tool
			break
		}
	}
	require.NotNil(t, historyTool, "cron loader did not register kata.cron_job_history")
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(mustJSON(t, historyTool.OutputSchema), &schema))
	require.Equal(t, "object", schema.Type)
	require.Contains(t, schema.Properties, "versions")
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	require.NoError(t, resolved.Validate(result.StructuredContent))
	properties := schema.Properties["versions"].Items.Properties
	require.Equal(t, "object", properties["definition"].Type)
	require.ElementsMatch(t, []string{"null", "array"}, properties["changes"].Types)
	require.Empty(t, properties["changes"].Items.Properties["before"].Type)
	require.Empty(t, properties["changes"].Items.Properties["after"].Type)
	raw := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": id, "diff": false})
	for _, v := range raw["versions"].([]any) {
		require.NotContains(t, v, "changes")
	}
}

func TestCronJobHistoryDiffLookahead(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			const identity = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
			const cursor = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			const older = "01ARZ3NDEKTSV4RRFFQ69G5FAX"
			const skippedFirst = "01ARZ3NDEKTSV4RRFFQ69G5FAY"
			const skippedOlder = "01ARZ3NDEKTSV4RRFFQ69G5FAZ"
			definition := cronDefinition()
			version := func(name, event string, current bool, counter int) map[string]any {
				return map[string]any{"event_uid": event, "name": name, "definition": definition, "actor": "example-agent", "operation": "updated", "updated_at": "2026-11-10T00:00:00Z", "definition_hlc": map[string]any{"version": 1, "physical_ms": int64(1794268800000), "counter": counter, "origin_instance_uid": identity}, "current": current}
			}
			requests := make(chan *http.Request, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests <- r
				w.Header().Set("Content-Type", "application/json")
				if len(requests) == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"versions": []any{version("New", cursor, true, 1)}, "next_before_event_uid": cursor, "skipped_event_uids": []string{skippedFirst}})
					return
				}
				if failed {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = fmt.Fprint(w, `{"error":{"code":"unavailable","message":"retry later"}}`)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"versions": []any{version("Old", older, false, 0)}, "skipped_event_uids": []string{skippedOlder}})
			}))
			t.Cleanup(server.Close)
			client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
			require.NoError(t, err)
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
			result := cronCall(t, session, "cron_job_history", map[string]any{"uid": identity, "limit": 1, "diff": true})
			require.Len(t, requests, 2)
			first, lookahead := <-requests, <-requests
			for _, request := range []*http.Request{first, lookahead} {
				require.Equal(t, http.MethodGet, request.Method)
				require.Equal(t, "/api/v1/projects/42/cron/jobs/"+identity+"/versions", request.URL.Path)
				require.Equal(t, "1", request.URL.Query().Get("limit"))
			}
			require.Empty(t, first.URL.Query().Get("before_event_uid"))
			require.Equal(t, cursor, lookahead.URL.Query().Get("before_event_uid"))
			if failed {
				require.True(t, result.IsError)
				require.Nil(t, result.StructuredContent)
				require.NotContains(t, result.Content[0].(*sdkmcp.TextContent).Text, `"versions"`)
				return
			}
			require.False(t, result.IsError, "%s", mustJSON(t, result))
			out := result.StructuredContent.(map[string]any)
			require.Equal(t, cursor, out["next_before_event_uid"])
			require.Equal(t, []any{skippedFirst, skippedOlder}, out["skipped_event_uids"])
			versions := out["versions"].([]any)
			require.Len(t, versions, 1)
			require.Equal(t, []any{map[string]any{"op": "replace", "path": "/name", "before": "Old", "after": "New"}}, versions[0].(map[string]any)["changes"])
		})
	}
}
