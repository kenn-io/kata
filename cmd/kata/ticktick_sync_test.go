package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func cliTickTickResponse() map[string]any {
	r := cliNotionResponse()
	b := r["binding"].(map[string]any)
	b["provider"] = "ticktick"
	b["source_key"] = "ticktick:project-1"
	b["remote_id"] = "project-1"
	b["config"] = map[string]any{"project_id": "project-1", "status_sync": "one-way", "title_prefix": false}
	r["status"].(map[string]any)["provider"] = "ticktick"
	return r
}
func TestTickTickSyncCommands(t *testing.T) {
	var paths []string
	env, dir := notionCLIHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/enable") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "10m", body["interval"])
			require.Equal(t, map[string]any{"project_id": "project-1", "title_prefix": false}, body["config"])
		}
		require.NoError(t, json.NewEncoder(w).Encode(cliTickTickResponse()))
	})
	require.Contains(t, runCLI(t, env, dir, "sync", "ticktick", "enable", "--ticktick-project", "project-1", "--interval", "10m", "--title-prefix=false"), "TickTick sync enabled")
	require.Contains(t, runCLI(t, env, dir, "--agent", "sync", "ticktick", "once"), "OK ticktick-sync action=once")
	require.Contains(t, runCLI(t, env, dir, "sync", "ticktick", "status"), "TickTick")
	require.Contains(t, runCLI(t, env, dir, "sync", "ticktick", "disable"), "TickTick sync disabled")
	require.Len(t, paths, 4)
}
func TestTickTickInvalidOptionsStayLocal(t *testing.T) {
	for _, args := range [][]string{{"--ticktick-project", "../project"}, {"--interval", "0"}, {"--interval", "1ms"}, {"--status-sync", "everything"}} {
		env, dir := notionCLIHTTP(t, func(http.ResponseWriter, *http.Request) { t.Fatal("invalid option reached daemon") })
		_, _, err := runCLIWithErr(t, env, dir, append([]string{"sync", "ticktick", "enable"}, args...)...)
		require.Error(t, err)
	}
}
