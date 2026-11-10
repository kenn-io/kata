package mcpserver

import (
	"encoding/json"
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
	callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer", "message": "Separate actor inbox"})
	cleared := callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, true, cleared["changed"])
	require.Empty(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})["requests"])
	require.Len(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})["requests"], 1)
	cleared = callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, false, cleared["changed"])
	root := callCoordinationTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Inspect this contract", "teammate": "planner", "idempotency_key": "linked-root"})["comment"].(map[string]any)
	shown := callCoordinationTool(t, session, "kata.show", map[string]any{"ref": ref})
	root["handle"] = shown["issue"].(map[string]any)["comments"].([]any)[0].(map[string]any)["handle"]
	callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "example-agent/worker", "message": "Please answer", "re": root["handle"], "teammate": "planner"})
	inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/worker"})
	require.Equal(t, root["uid"], inbox["requests"].([]any)[0].(map[string]any)["re"])
	reply := callCoordinationTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Confirmed the notification contract against the isolated fixture.", "reply_to": root["handle"], "kind": "confirm", "teammate": "worker", "idempotency_key": "linked-answer"})["comment"].(map[string]any)
	inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/planner"})
	require.Len(t, inbox["requests"], 1)
	request = inbox["requests"].([]any)[0].(map[string]any)
	require.Equal(t, reply["uid"], request["re"])
	require.Equal(t, "confirm", request["kind"])
}

func TestMCPNotifyBroadcast(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Coordinate review", "idempotency_key": "broadcast-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	callCoordinationTool(t, session, "kata.assign", map[string]any{"ref": ref, "owner": "reviewer"})
	args := map[string]any{"ref": ref, "broadcast": true, "message": "Please inspect the update"}
	sent := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, []any{"reviewer"}, sent["recipients"])
	inbox := callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})
	require.Equal(t, true, inbox["requests"].([]any)[0].(map[string]any)["broadcast"])
}

func TestMCPNotifyValidationAndScope(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Keep untouched", "idempotency_key": "validation-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"clear message", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "message": "x"}, "not: validated against"},
		{"clear linked", map[string]any{"ref": ref, "to": "reviewer", "clear": true, "re": "c:abcdef"}, "not: validated against"},
		{"clear broadcast", map[string]any{"ref": ref, "clear": true, "broadcast": true}, "clear cannot"},
		{"clear invalid recipient", map[string]any{"ref": ref, "to": "review\ner", "clear": true}, "recipient"},
		{"outside scope", map[string]any{"ref": "other-project#abcd", "to": "reviewer", "message": "x"}, "outside"},
		{"forged sender", map[string]any{"ref": ref, "to": "reviewer", "message": "x", "from": "someone-else"}, "additional properties"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "clear message" || tc.name == "clear linked" {
				control := make(map[string]any, len(tc.args))
				for k, v := range tc.args {
					control[k] = v
				}
				if tc.name == "clear message" {
					delete(control, "message")
				} else {
					delete(control, "re")
				}
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

func TestMCPNotifyIdentityAndTeammateDefaults(t *testing.T) {
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
}

func TestMCPNotifyOlderDaemon(t *testing.T) {
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects" {
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
			return
		}
		http.NotFound(w, r)
	})
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.notify", Arguments: map[string]any{"ref": "abc1", "to": "reviewer", "message": "Please review"}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, string(mustJSON(t, result.Content)), "daemon does not support notifications")
}
