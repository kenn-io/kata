package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const cliTwentyProject = "11111111-1111-4111-8111-111111111111"

func cliTwentyResponse() map[string]any {
	r := cliNotionResponse()
	b := r["binding"].(map[string]any)
	b["provider"] = "twenty"
	b["source_key"] = "twenty:https://api.twenty.com/" + cliTwentyProject
	b["remote_id"] = cliTwentyProject
	b["config"] = map[string]any{"api_origin": "https://api.twenty.com", "web_origin": "https://app.twenty.com", "workspace_id": cliTwentyProject, "closed_status": "DONE", "open_status": "TODO", "open_statuses": []string{"TODO", "IN_PROGRESS"}, "since": "2026-01-01T00:00:00Z", "title_prefix": false}
	r["status"].(map[string]any)["provider"] = "twenty"
	return r
}
func TestTwentySyncCommands(t *testing.T) {
	var paths []string
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/enable") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "10m", body["interval"])
			require.Equal(t, map[string]any{"closed_status": "DONE", "open_status": "TODO", "open_statuses": []any{"TODO", "IN_PROGRESS"}, "title_prefix": false}, body["config"])
		}
		require.NoError(t, json.NewEncoder(w).Encode(cliTwentyResponse()))
	})
	require.Contains(t, runCLI(t, env, dir, "sync", "twenty", "enable", "--closed-status", "DONE", "--open-status", "TODO", "--open-statuses", "TODO,IN_PROGRESS", "--interval", "10m", "--title-prefix=false"), "Twenty sync enabled")
	require.Contains(t, runCLI(t, env, dir, "--agent", "sync", "twenty", "once"), "OK twenty-sync action=once")
	out := runCLI(t, env, dir, "--json", "sync", "twenty", "status")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Equal(t, "twenty", body["binding"].(map[string]any)["provider"])
	require.Contains(t, runCLI(t, env, dir, "sync", "twenty", "disable"), "Twenty sync disabled")
	for i, a := range []string{"enable", "once", "status", "disable"} {
		m := "POST"
		if a == "status" {
			m = "GET"
		}
		require.Regexp(t, fmt.Sprintf(`^%s /api/v1/projects/\d+/issue-sync/twenty/%s$`, m, a), paths[i])
	}
}
func TestTwentyOptionsPresence(t *testing.T) {
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
			require.NoError(t, json.NewEncoder(w).Encode(cliTwentyResponse()))
		})
		runCLI(t, env, dir, append([]string{"sync", "twenty", "enable"}, tc.args...)...)
	}
}
func TestTwentyInvalidInputsRejectBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"--open-status", ""}, {"--closed-status", ""}, {"--open-statuses", ""}, {"--open-statuses", "TODO,TODO"}, {"--since", "0000-01-01"}, {"--since", "bad"}, {"--interval", "0"}, {"--interval", "bad"}, {"--interval", "1ms"}, {"--token", "secret"}, {"--api-origin", "https://twenty.example"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached daemon") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "twenty", "enable"}, args...)...)
		require.Error(t, err)
	}
}
func TestTwentyStatusModes(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		r := cliTwentyResponse()
		r["binding"].(map[string]any)["display_name"] = "Tasks\nInjected\x1b[31m"
		s := r["status"].(map[string]any)
		s["state"] = "running"
		s["progress"] = map[string]any{"phase": "tasks", "completed": 1, "total": 2, "started_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
		env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(r)) })
		args := []string{"sync", "twenty", "status"}
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
				for _, want := range []string{"Twenty sync running", "API origin:", "Web origin:", "Workspace:", "Closed target: DONE", "Open statuses: TODO,IN_PROGRESS", "Title prefix: false", "Progress:"} {
					require.Contains(t, out, want)
				}
			} else {
				require.Contains(t, out, "workspace_id=11111111-1111-4111-8111-111111111111")
				require.Contains(t, out, "phase=tasks")
			}
		}
	}
}
