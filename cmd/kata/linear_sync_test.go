package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const cliLinearProject = "11111111-1111-4111-8111-111111111111"
const cliLinearItem = "33333333-3333-4333-8333-333333333333"

func cliLinearResponse() map[string]any {
	r := cliNotionResponse()
	b := r["binding"].(map[string]any)
	b["provider"] = "linear"
	b["source_key"] = "linear:" + cliLinearItem + "/" + cliLinearProject
	b["remote_id"] = cliLinearItem + "/" + cliLinearProject
	b["config"] = map[string]any{"workspace_id": cliLinearItem, "team_id": cliLinearProject, "since": "2026-01-01T00:00:00Z", "title_prefix": false}
	r["status"].(map[string]any)["provider"] = "linear"
	return r
}
func TestLinearSyncCommands(t *testing.T) {
	var paths []string
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/enable") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "10m", body["interval"])
			require.Equal(t, map[string]any{"workspace_id": cliLinearItem, "team_id": cliLinearProject, "title_prefix": false}, body["config"])
		}
		require.NoError(t, json.NewEncoder(w).Encode(cliLinearResponse()))
	})
	require.Contains(t, runCLI(t, env, dir, "sync", "linear", "enable", "--linear-workspace", cliLinearItem, "--linear-team", cliLinearProject, "--interval", "10m", "--title-prefix=false"), "Linear sync enabled")
	require.Contains(t, runCLI(t, env, dir, "--agent", "sync", "linear", "once"), "OK linear-sync action=once")
	out := runCLI(t, env, dir, "--json", "sync", "linear", "status")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &body))
	require.Equal(t, "linear", body["binding"].(map[string]any)["provider"])
	require.Contains(t, runCLI(t, env, dir, "sync", "linear", "disable"), "Linear sync disabled")
	for i, a := range []string{"enable", "once", "status", "disable"} {
		m := "POST"
		if a == "status" {
			m = "GET"
		}
		require.Regexp(t, fmt.Sprintf(`^%s /api/v1/projects/\d+/issue-sync/linear/%s$`, m, a), paths[i])
	}
}
func TestLinearOptionsPresence(t *testing.T) {
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
			require.NoError(t, json.NewEncoder(w).Encode(cliLinearResponse()))
		})
		runCLI(t, env, dir, append([]string{"sync", "linear", "enable"}, tc.args...)...)
	}
}
func TestLinearInvalidInputsRejectBeforeDaemon(t *testing.T) {
	for _, args := range [][]string{{"--linear-workspace", ""}, {"--linear-workspace", "bad/slug"}, {"--linear-project", "bad"}, {"--linear-project", "https://linear.example/project"}, {"--since", "0000-01-01"}, {"--since", "bad"}, {"--interval", "0"}, {"--interval", "bad"}, {"--interval", "1ms"}, {"--token", "secret"}, {"--api-origin", "https://linear.example"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached daemon") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "linear", "enable"}, args...)...)
		require.Error(t, err)
	}
}
func TestLinearStatusModes(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		r := cliLinearResponse()
		r["binding"].(map[string]any)["display_name"] = "Tasks\nInjected\x1b[31m"
		s := r["status"].(map[string]any)
		s["state"] = "running"
		s["progress"] = map[string]any{"phase": "issues", "completed": 1, "total": 2, "started_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:01:00Z"}
		env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, _ *http.Request) { require.NoError(t, json.NewEncoder(w).Encode(r)) })
		args := []string{"sync", "linear", "status"}
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
				for _, want := range []string{"Linear sync running", "Workspace:", "Team:", "Title prefix: false", "Progress:"} {
					require.Contains(t, out, want)
				}
			} else {
				require.Contains(t, out, "workspace_id=33333333-3333-4333-8333-333333333333")
				require.Contains(t, out, "phase=issues")
			}
		}
	}
}
