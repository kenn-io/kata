package mcpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestCronJobHistoryDiscovery(t *testing.T) {
	s := connectRawTestServerWithOptions(t, Options{Client: inertTestClient(t), ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	loaded := cronOK(t, s, "load_cron", map[string]any{})
	require.Contains(t, loaded["tools"], "kata.cron_job_history")
	after, err := s.ListTools(t.Context(), nil)
	require.NoError(t, err)
	for _, tool := range after.Tools {
		if tool.Name != "kata.cron_job_history" {
			continue
		}
		require.True(t, tool.Annotations.ReadOnlyHint)
		var schema map[string]any
		require.NoError(t, json.Unmarshal(mustJSON(t, tool.OutputSchema), &schema))
		require.Equal(t, "object", schema["type"])
		require.Contains(t, schema["properties"], "versions")
		return
	}
	t.Fatal("cron loader did not register kata.cron_job_history")
}

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
	require.Equal(t, "deleted", latest["operation"])
	require.Equal(t, true, latest["current"])
	require.Equal(t, "example-agent", latest["actor"])
	require.NotNil(t, latest["deleted_at"])
	require.Equal(t, latest["event_uid"], page["next_before_event_uid"])

	older := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": id, "limit": 100, "before_event_uid": strings.ToLower(page["next_before_event_uid"].(string))})
	versions = older["versions"].([]any)
	require.Len(t, versions, 2)
	for i, want := range []map[string]any{updated, created} {
		version := versions[i].(map[string]any)
		require.Equal(t, want["definition_event_uid"], version["event_uid"])
		require.Equal(t, want["name"], version["name"])
		require.Equal(t, false, version["current"])
	}
	require.NotContains(t, older, "next_before_event_uid")
	all := cronOK(t, f.session, "cron_job_history", map[string]any{"uid": id})
	require.Len(t, all["versions"], 3)

	other, err := f.store.CreateProject(t.Context(), "other-project")
	require.NoError(t, err)
	outside := cronCall(t, f.session, "cron_job_history", map[string]any{"uid": id, "project": other.Name})
	require.True(t, outside.IsError)
	require.Contains(t, outside.Content[0].(*sdkmcp.TextContent).Text, "outside")
	allowed := connectTestServerWithOptions(t, Options{Client: f.client, Scope: NewAllScope(), Actor: "example-agent", Version: "test"})
	require.True(t, cronCall(t, allowed, "cron_job_history", map[string]any{"uid": id}).IsError, "multi-project scope requires project")
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
		{"negative limit", map[string]any{"uid": id, "limit": -1}, "limit"},
		{"large limit", map[string]any{"uid": id, "limit": 101}, "limit"},
		{"bad cursor", map[string]any{"uid": id, "before_event_uid": "short"}, "ULID"},
		{"unknown cursor", map[string]any{"uid": id, "before_event_uid": cronNewUID(t, f.session)}, "validation"},
		{"unknown job", map[string]any{"uid": cronNewUID(t, f.session)}, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := cronCall(t, f.session, "cron_job_history", tc.args)
			require.True(t, result.IsError)
			require.Contains(t, result.Content[0].(*sdkmcp.TextContent).Text, tc.want)
		})
	}
	_, _, err := f.store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: f.project.ID, Actor: "example-agent"})
	require.NoError(t, err)
	require.True(t, cronCall(t, f.session, "cron_job_history", map[string]any{"uid": id}).IsError)
}

// A wire fixture exercises metadata that ordinary local edits do not produce:
// baselines, skipped-event diagnostics, and arbitrary opaque numeric literals.
func TestCronJobHistoryPreservesResponse(t *testing.T) {
	var body atomic.Value
	body.Store(`{"versions":[]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/projects/42/cron/jobs/01ARZ3NDEKTSV4RRFFQ69G5FAV/versions", r.URL.Path)
		require.NotContains(t, r.URL.Query(), "limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(server.Close)
	client, err := kataclient.NewWithHTTPClient(server.URL, server.Client())
	require.NoError(t, err)
	s := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 42, ProjectName: "spoke-project", Actor: "example-agent", Version: "test"})
	check := func(rt require.TestingT, integer, decimal string) {
		raw := fmt.Sprintf(`{"versions":[{"actor":null,"current":false,"definition":{"integer":%s,"decimal":%s},"definition_hlc":{"counter":2,"origin_instance_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","physical_ms":3,"version":1},"event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","name":"Baseline","operation":"baseline","updated_at":"2026-11-10T00:00:00Z"}],"next_before_event_uid":"01ARZ3NDEKTSV4RRFFQ69G5FAW","skipped_event_uids":["01ARZ3NDEKTSV4RRFFQ69G5FAX"]}`, integer, decimal)
		body.Store(raw)
		result, err := s.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.cron_job_history", Arguments: map[string]any{"uid": "01ARZ3NDEKTSV4RRFFQ69G5FAV"}})
		require.NoError(rt, err)
		require.False(rt, result.IsError)
		text := result.Content[0].(*sdkmcp.TextContent).Text
		var want, got any
		decode := func(raw string, target *any) {
			d := json.NewDecoder(strings.NewReader(raw))
			d.UseNumber()
			require.NoError(rt, d.Decode(target))
		}
		decode(raw, &want)
		decode(text, &got)
		require.Equal(rt, want, got)
	}
	check(t, "9007199254740993", "1.0000000000000000001")
}
