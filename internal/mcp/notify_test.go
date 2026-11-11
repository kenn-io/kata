package mcpserver

import (
	"encoding/json"
	"maps"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Typed sends and clears address one exact recipient.
func TestMCPNotifySendAndClearAgainstDaemon(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Review the adapter", "idempotency_key": "notify-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	args := map[string]any{"ref": ref, "to": " reviewer/worker ", "message": "Please review"}
	sent := callAdministrationTool(t, session, "kata.notify", args)
	require.Equal(t, true, sent["changed"])
	require.Equal(t, []any{"reviewer/worker"}, sent["recipients"])
	require.Equal(t, "example-agent", sent["event"].(map[string]any)["actor"])
	inbox := callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})
	require.Len(t, inbox["requests"], 1)
	request := inbox["requests"].([]any)[0].(map[string]any)
	require.Equal(t, "Please review", request["message"])
	cleared := callAdministrationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, true, cleared["changed"])
	require.Empty(t, callAdministrationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})["requests"])
	cleared = callAdministrationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, false, cleared["changed"])
}

func TestMCPNotifyValidation(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Validation", "idempotency_key": "validation-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	for _, tc := range []struct {
		name, want, without string
		args                map[string]any
	}{
		{"empty message", "message must be nonblank", "", map[string]any{"ref": ref, "to": "reviewer", "message": ""}},
		{"clear message", "not: validated against", "message", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "message": "x"}},
		{"clear linked", "not: validated against", "re", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "re": "c:abcdef"}},
		{"clear broadcast", "clear cannot", "", map[string]any{"ref": ref, "clear": true, "broadcast": true}},
		{"clear invalid recipient", "recipient", "", map[string]any{"ref": ref, "to": "review\ner", "clear": true}},
		{"forged sender", "additional properties", "", map[string]any{"ref": ref, "to": "reviewer", "message": "x", "from": "someone-else"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.without != "" {
				control := maps.Clone(tc.args)
				delete(control, tc.without)
				callAdministrationTool(t, session, "kata.notify", control)
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

func TestMCPNotifyForwardsFieldsToDaemon(t *testing.T) {
	requests := make(chan map[string]any, 1)
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/projects/1/issues/abc1/notifications":
			var body map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			requests <- body
			writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "abc1"), "changed": false, "recipients": []string{"recipient"}})
		default:
			http.NotFound(w, r)
		}
	})
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Teammate: "default-worker", Version: "test"})
	for _, tc := range []struct {
		name  string
		args  map[string]any
		field string
		want  any
	}{
		{"startup", map[string]any{"to": "recipient"}, "teammate", "default-worker"},
		{"override", map[string]any{"to": "recipient", "teammate": "reviewer"}, "teammate", "reviewer"},
		{"suppressed", map[string]any{"to": "recipient", "teammate": ""}, "teammate", ""},
		{"linked", map[string]any{"to": "recipient", "re": "c:abcdef"}, "re", "c:abcdef"},
		{"broadcast", map[string]any{"broadcast": true}, "broadcast", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"ref": "abc1", "message": tc.name}
			maps.Copy(args, tc.args)
			callAdministrationTool(t, session, "kata.notify", args)
			body := <-requests
			require.Equal(t, tc.want, body[tc.field])
		})
	}
}
