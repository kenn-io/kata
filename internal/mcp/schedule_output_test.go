package mcpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	kataclient "go.kenn.io/kata/pkg/client"
)

func TestToolsForwardSavedSchedulesFromNewerDaemon(t *testing.T) {
	var writes atomic.Int32
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if discoveryHealth(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var response map[string]any
		require.NoError(t, json.Unmarshal(daemonResponse(r), &response))
		var issue map[string]any
		if r.Method == http.MethodPatch {
			writes.Add(1)
			issue = response["issue"].(map[string]any)
			issue["revision"] = 8
		} else if issues, ok := response["issues"].([]any); ok {
			issue = issues[0].(map[string]any)
			issue["scheduled_on_date"] = "2025-12-31"
			issue["deadline_on_date"] = "2026-01-02"
		} else if item, ok := response["issue"].(map[string]any); ok {
			issue = item
		}
		if issue != nil {
			issue["metadata"] = map[string]any{"timezone": "Asia/Tokyo", "scheduled_on": map[string]any{"value": "2026-01-01T02:00:00Z", "timezone": "America/New_York", "tzdata_version": "future"}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(response))
	}))
	t.Cleanup(daemon.Close)
	client, err := kataclient.NewWithHTTPClient(daemon.URL, daemon.Client())
	require.NoError(t, err)
	session := connectTestServerWithClient(t, client)
	edit, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.edit", Arguments: map[string]any{"ref": "abc1", "title": "Updated"}})
	require.NoError(t, err)
	require.False(t, edit.IsError, string(mustJSON(t, edit.Content)))
	require.EqualValues(t, 1, writes.Load())
	output := edit.StructuredContent.(map[string]any)
	require.Equal(t, true, output["changed"])
	issue := output["issue"].(map[string]any)
	require.EqualValues(t, 8, issue["revision"])
	require.Equal(t, "2026-01-01T02:00:00Z", issue["scheduled_on"])
	require.Equal(t, "Asia/Tokyo", issue["timezone"])
	require.Equal(t, "America/New_York", issue["scheduled_on_timezone"])
	require.Equal(t, "01ARZ3NDEKTSV4RRFFQ69G5FAW", output["event"].(map[string]any)["uid"])
	read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.list", Arguments: map[string]any{}})
	require.NoError(t, err)
	require.False(t, read.IsError, read.Content)
	issue = read.StructuredContent.(map[string]any)["issues"].([]any)[0].(map[string]any)
	require.Equal(t, "2025-12-31", issue["scheduled_on_date"])
	require.Equal(t, "2026-01-02", issue["deadline_on_date"])
	require.Equal(t, "Asia/Tokyo", issue["timezone"])
	require.Equal(t, "America/New_York", issue["scheduled_on_timezone"])
}
