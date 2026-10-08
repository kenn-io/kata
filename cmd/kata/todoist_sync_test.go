package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func cliTodoistResponse() map[string]any {
	return map[string]any{"binding": map[string]any{"id": 1, "project_id": 1, "provider": "todoist", "source_key": "todoist:https://api.todoist.com/1234567/project123", "display_name": "Example tasks", "config": map[string]any{"project_id": "project123", "account_id": "1234567", "history_since": "2026-09-01T00:00:00Z", "status_sync": "one-way", "title_prefix": true}, "enabled": true, "interval_seconds": 300}, "status": map[string]any{"binding_id": 1, "state": "idle"}}
}
func TestTodoistCLIEnablePreservesPresence(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{{nil, map[string]any{}}, {[]string{"--todoist-project", "project123", "--history-since", "2026-09-01", "--title-prefix=false"}, map[string]any{"project_id": "project123", "history_since": "2026-09-01T00:00:00Z", "title_prefix": false}}} {
		env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			require.Regexp(t, `^/api/v1/projects/\d+/issue-sync/todoist/enable$`, r.URL.Path)
			var body struct {
				Config map[string]any `json:"config"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			if len(tc.want) == 0 {
				require.Empty(t, body.Config)
			} else {
				require.Equal(t, tc.want, body.Config)
			}
			require.NoError(t, json.NewEncoder(w).Encode(cliTodoistResponse()))
		})
		out := runCLI(t, env, dir, append([]string{"sync", "todoist", "enable"}, tc.args...)...)
		require.Contains(t, out, "Todoist")
	}
}
func TestTodoistCLIRejectsUnsafeScope(t *testing.T) {
	for _, args := range [][]string{{"--todoist-project", "bad/path"}, {"--history-since", ""}, {"--history-since", "bad"}, {"--interval", "0"}, {"--token", "secret"}, {"--api-origin", "https://foreign.example"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached daemon") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "todoist", "enable"}, args...)...)
		require.Error(t, err)
	}
}
