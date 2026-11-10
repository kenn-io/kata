package mcpserver

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func callCoordinationTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s: %s", name, mustJSON(t, result.Content))
	var out map[string]any
	require.NoError(t, json.Unmarshal(mustJSON(t, result.StructuredContent), &out))
	return out
}

// Typed sends and clears address one exact recipient.
func TestMCPNotifySendAndClearAgainstDaemon(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Review the adapter", "idempotency_key": "notify-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	args := map[string]any{"ref": ref, "to": " reviewer/worker ", "message": "Please review", "teammate": "planner"}
	sent := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, true, sent["changed"])
	require.Equal(t, []any{"reviewer/worker"}, sent["recipients"])
	require.Equal(t, "example-agent", sent["event"].(map[string]any)["actor"])
	inbox := callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})
	require.Len(t, inbox["requests"], 1)
	request := inbox["requests"].([]any)[0].(map[string]any)
	require.Equal(t, "Please review", request["message"])
	cleared := callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, true, cleared["changed"])
	require.Empty(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})["requests"])
	cleared = callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, false, cleared["changed"])

}

func TestMCPNotifyValidationAndScope(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Keep untouched", "idempotency_key": "validation-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	for _, tc := range []struct {
		name, want, without string
		args                map[string]any
	}{
		{"clear message", "not: validated against", "message", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "message": "x"}},
		{"clear linked", "not: validated against", "re", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "re": "c:abcdef"}},
		{"clear broadcast", "clear cannot", "", map[string]any{"ref": ref, "clear": true, "broadcast": true}},
		{"broadcast recipient", "mutually exclusive", "", map[string]any{"ref": ref, "to": "reviewer", "broadcast": true, "message": "x"}},
		{"clear invalid recipient", "recipient", "", map[string]any{"ref": ref, "to": "review\ner", "clear": true}},
		{"forged sender", "additional properties", "", map[string]any{"ref": ref, "to": "reviewer", "message": "x", "from": "someone-else"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.without != "" {
				control := maps.Clone(tc.args)
				delete(control, tc.without)
				callCoordinationTool(t, session, "kata.notify", control)
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.notify", Arguments: tc.args})
			if err != nil {
				require.Contains(t, err.Error(), tc.want)
				return
			}
			require.True(t, result.IsError)
			require.Contains(t, string(mustJSON(t, result.Content)), tc.want)
		})
	}
}

func TestMCPNotifyTeammateDefaults(t *testing.T) {
	requests := make(chan map[string]any, 1)
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/projects/1/issues/abc1/notifications":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			requests <- body
			writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "abc1"), "changed": false, "recipients": []string{"recipient"}})
		default:
			http.NotFound(w, r)
		}
	})
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Teammate: "default-worker", Version: "test"})
	for _, tc := range []struct {
		name     string
		teammate *string
		want     string
	}{
		{"startup", nil, "default-worker"}, {"override", new("reviewer"), "reviewer"}, {"suppressed", new(""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"ref": "abc1", "to": "recipient", "message": tc.name}
			if tc.teammate != nil {
				args["teammate"] = *tc.teammate
			}
			callCoordinationTool(t, session, "kata.notify", args)
			body := <-requests
			require.Equal(t, tc.want, body["teammate"])
		})
	}
	for _, args := range []map[string]any{
		{"ref": "abc1", "to": "recipient", "message": "linked", "re": "c:abcdef"},
		{"ref": "abc1", "broadcast": true, "message": "broadcast"},
	} {
		callCoordinationTool(t, session, "kata.notify", args)
		body := <-requests
		if re, ok := args["re"]; ok {
			require.Equal(t, re, body["re"])
		}
		if broadcast, ok := args["broadcast"]; ok {
			require.Equal(t, broadcast, body["broadcast"])
		}
	}

}
