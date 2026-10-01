package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/planesync"
	"go.kenn.io/kata/internal/testenv"
)

const cliPlaneProject = "11111111-1111-4111-8111-111111111111"
const cliPlaneState = "22222222-2222-4222-8222-222222222222"
const cliPlaneItem = "33333333-3333-4333-8333-333333333333"

func cliPlaneResponse() map[string]any {
	r := cliNotionResponse()
	b := r["binding"].(map[string]any)
	b["provider"] = "plane"
	b["source_key"] = "plane:https://api.plane.so/example-workspace/" + cliPlaneProject
	b["remote_id"] = "example-workspace/" + cliPlaneProject
	b["config"] = map[string]any{"api_origin": "https://api.plane.so", "web_origin": "https://app.plane.so", "workspace": "example-workspace", "project_id": cliPlaneProject, "since": "2026-01-01T00:00:00Z", "title_prefix": false}
	r["status"].(map[string]any)["provider"] = "plane"
	return r
}
func TestPlaneSyncCommands(t *testing.T) {
	var paths []string
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/enable") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "10m", body["interval"])
			require.Equal(t, map[string]any{"workspace": "example-workspace", "project_id": cliPlaneProject, "title_prefix": false}, body["config"])
		}
		require.NoError(t, json.NewEncoder(w).Encode(cliPlaneResponse()))
	})
	require.Contains(t, runCLI(t, env, dir, "sync", "plane", "enable", "--plane-workspace", "example-workspace", "--plane-project", cliPlaneProject, "--interval", "10m", "--title-prefix=false"), "Plane sync enabled")
	require.Contains(t, runCLI(t, env, dir, "--agent", "sync", "plane", "once"), "OK plane-sync action=once")
	out := runCLI(t, env, dir, "--json", "sync", "plane", "status")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Equal(t, "plane", body["binding"].(map[string]any)["provider"])
	require.Contains(t, runCLI(t, env, dir, "sync", "plane", "disable"), "Plane sync disabled")
	for i, a := range []string{"enable", "once", "status", "disable"} {
		m := "POST"
		if a == "status" {
			m = "GET"
		}
		require.Regexp(t, fmt.Sprintf(`^%s /api/v1/projects/\d+/issue-sync/plane/%s$`, m, a), paths[i])
	}
}
func TestPlaneOptionsPresence(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{{nil, map[string]any{}}, {[]string{"--since", "", "--title-prefix=false"}, map[string]any{"since": "", "title_prefix": false}}, {[]string{"--since", "2026-01-02T03:00:00+03:00"}, map[string]any{"since": "2026-01-02T00:00:00Z"}}} {
		env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Config map[string]any `json:"config"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if len(tc.want) == 0 {
				require.Empty(t, body.Config)
			} else {
				require.Equal(t, tc.want, body.Config)
			}
			require.NoError(t, json.NewEncoder(w).Encode(cliPlaneResponse()))
		})
		runCLI(t, env, dir, append([]string{"sync", "plane", "enable"}, tc.args...)...)
	}
}
func TestPlaneInvalidInputsRejectBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"--plane-workspace", ""}, {"--plane-workspace", "bad/slug"}, {"--plane-project", "bad"}, {"--plane-project", "https://plane.example/project"}, {"--since", "0000-01-01"}, {"--since", "bad"}, {"--interval", "0"}, {"--interval", "bad"}, {"--interval", "1ms"}, {"--token", "secret"}, {"--api-origin", "https://plane.example"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached daemon") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "plane", "enable"}, args...)...)
		require.Error(t, err)
	}
}
func TestPlaneStatusModes(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		r := cliPlaneResponse()
		r["binding"].(map[string]any)["display_name"] = "Tasks\nInjected\x1b[31m"
		s := r["status"].(map[string]any)
		s["state"] = "running"
		s["progress"] = map[string]any{"phase": "work-items", "completed": 1, "total": 2, "started_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
		env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(r)) })
		args := []string{"sync", "plane", "status"}
		if mode != "human" {
			args = append([]string{"--" + mode}, args...)
		}
		out := runCLI(t, env, dir, args...)
		require.NotContains(t, out, "token_env")
		if mode == "json" {
			var decoded map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &decoded))
		} else {
			require.NotContains(t, out, "\nInjected")
			require.NotContains(t, out, "\x1b")
			if mode == "human" {
				for _, want := range []string{"Plane sync running", "API origin:", "Web origin:", "Workspace:", "Plane project:", "Title prefix: false", "Progress:"} {
					require.Contains(t, out, want)
				}
			} else {
				require.Contains(t, out, "workspace=example-workspace")
				require.Contains(t, out, "phase=work-items")
			}
		}
	}
}
func TestPlaneReadOnlyAcceptance(t *testing.T) {
	t.Setenv("KATA_PLANE_TOKEN", "")
	t.Setenv("KATA_HTTP_TIMEOUT", "100ms")
	var group atomic.Value
	group.Store("started")
	var reads atomic.Int32
	token := "example-daemon-plane-key"
	transport := cliNotionTransport(func(r *http.Request) (*http.Response, error) {
		reads.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "https://api.plane.so", r.URL.Scheme+"://"+r.URL.Host)
		require.Equal(t, token, r.Header.Get("X-API-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		base := "/api/v1/workspaces/example-workspace/projects/" + cliPlaneProject + "/"
		var body any
		switch r.URL.Path {
		case base:
			body = map[string]any{"id": cliPlaneProject, "name": "Example tasks", "identifier": "EX"}
		case base + "states/":
			body = []any{map[string]any{"id": cliPlaneState, "group": group.Load()}}
		case base + "work-items/":
			body = map[string]any{"results": []any{map[string]any{"id": cliPlaneItem, "project": cliPlaneProject, "state": cliPlaneState, "sequence_id": 1, "priority": "high", "name": "Imported task", "description_html": "<p>Task <strong>body</strong></p>", "created_by": nil, "assignees": []any{}, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}}, "next_page_results": false, "next_cursor": "final"}
		default:
			return nil, fmt.Errorf("unexpected Plane route")
		}
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
	})
	client := planesync.NewClient(planesync.ClientConfig{Transport: transport, LookupEnv: func(string) (string, bool) { return token, true }})
	env := testenv.New(t, func(c *daemon.ServerConfig) { c.PlaneSyncFetcher = client })
	dir := initBoundWorkspace(t, env.URL, "https://daemon.example/spoke-project.git")
	id := resolvePIDViaHTTP(t, env.URL, dir)
	require.Contains(t, runCLI(t, env, dir, "sync", "plane", "enable", "--plane-workspace", "example-workspace", "--plane-project", cliPlaneProject), "Plane sync enabled")
	require.Contains(t, runCLI(t, env, dir, "sync", "plane", "once"), "created=1")
	items, err := env.DB.ListIssues(context.Background(), db.ListIssuesParams{ProjectID: id})
	require.NoError(t, err)
	require.Len(t, items, 1)
	i := items[0]
	require.Equal(t, "[Plane EX-1] Imported task", i.Title)
	require.Equal(t, new(int64(1)), i.Priority)
	require.Contains(t, i.Body, "Task **body**")
	group.Store("completed")
	require.Contains(t, runCLI(t, env, dir, "sync", "plane", "once"), "updated=1")
	closed, err := env.DB.IssueByID(context.Background(), i.ID)
	require.NoError(t, err)
	require.Equal(t, "closed", closed.Status)
	require.True(t, i.UpdatedAt.Equal(closed.UpdatedAt))
	before := reads.Load()
	runCLI(t, env, dir, "reopen", i.ShortID)
	runCLI(t, env, dir, "close", i.ShortID, "--done", "--message", "Completed the imported task and checked its disposable acceptance flow.", "--commit", "deadbeef")
	require.Equal(t, before, reads.Load())
	require.NotContains(t, runCLI(t, env, dir, "sync", "plane", "status"), token)
}
