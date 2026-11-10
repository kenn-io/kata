package mcpserver

import (
	"encoding/json"
	"encoding/json/jsontext"
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
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
	session, store := newCoordinationSession(t)
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
	root := callCoordinationTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Inspect this contract", "teammate": "planner", "idempotency_key": "linked-root"})["comment"].(map[string]any)
	shown := callCoordinationTool(t, session, "kata.show", map[string]any{"ref": ref})
	root["handle"] = shown["issue"].(map[string]any)["comments"].([]any)[0].(map[string]any)["handle"]
	callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "example-agent/worker", "message": "Please answer", "re": root["handle"], "teammate": "planner"})
	inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/worker"})
	require.Equal(t, root["uid"], inbox["requests"].([]any)[0].(map[string]any)["re"])
	issue, err := store.IssueByUID(t.Context(), created["issue"].(map[string]any)["uid"].(string), db.IncludeDeletedNo)
	require.NoError(t, err)
	value, err := json.Marshal(map[string]any{"from": "worker", "message": "Confirmed", "re": root["uid"], "kind": "confirm"})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{notification.MetadataKey("example-agent/planner"): value}})
	require.NoError(t, err)
	inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/planner"})
	require.Len(t, inbox["requests"], 1)
	request = inbox["requests"].([]any)[0].(map[string]any)
	require.Equal(t, root["uid"], request["re"])
	require.Equal(t, "confirm", request["kind"])
	callCoordinationTool(t, session, "kata.assign", map[string]any{"ref": ref, "owner": "reviewer"})
	args = map[string]any{"ref": ref, "broadcast": true, "message": "Please inspect the update"}
	sent = callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, []any{"reviewer"}, sent["recipients"])
	inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})
	require.Equal(t, true, inbox["requests"].([]any)[0].(map[string]any)["broadcast"])
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
		{"clear invalid recipient", "recipient", "", map[string]any{"ref": ref, "to": "review\ner", "clear": true}},
		{"outside scope", "outside", "", map[string]any{"ref": "other-project#abcd", "to": "reviewer", "message": "x"}},
		{"forged sender", "additional properties", "", map[string]any{"ref": ref, "to": "reviewer", "message": "x", "from": "someone-else"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.without != "" {
				control := make(map[string]any, len(tc.args))
				for k, v := range tc.args {
					control[k] = v
				}
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

func TestMCPNotifyHistoricalReferenceOutsideScope(t *testing.T) {
	store, _, session, source, _, excluded, issue := mcpFixedScopeFixture(t)
	args := map[string]any{"ref": source.Name + "#" + issue.ShortID, "to": "reviewer", "message": "First request"}
	sent := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, true, sent["changed"])
	issue, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	mcpMoveOutside(t, store, source, excluded, issue)
	before, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	beforeEvent, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	args["message"] = "Out of scope request"
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.notify", Arguments: args})
	require.NoError(t, err)
	require.True(t, result.IsError, string(mustJSON(t, result)))
	after, err := store.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.Equal(t, before.Metadata, after.Metadata)
	afterEvent, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, beforeEvent, afterEvent)
}
