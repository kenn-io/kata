package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/testenv"
	kataclient "go.kenn.io/kata/pkg/client"
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

// Contract: assistants reading shipped teammate guidance can discover the
// native notification tool and attention/reply wait arguments.
func TestMCPCoordinationGuideMapsNativeAdapters(t *testing.T) {
	rows := map[string]SkillCommand{}
	for _, row := range SkillCommands() {
		rows[row.CLI] = row
	}
	require.Equal(t, []string{"kata.notify"}, rows["kata notify"].Tools)
	require.Equal(t, []string{"kata.wait"}, rows["kata wait"].Tools)
	require.Contains(t, rows["kata wait"].Note, "`until`")
	require.Contains(t, rows["kata wait"].Note, "`reply`")
}

// Contract: typed sends and clears address one exact recipient, retry without
// adding events, and never change delegated work.attention.
func TestMCPNotifySendRetryAndClearAgainstDaemon(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Review the adapter", "idempotency_key": "notify-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	callCoordinationTool(t, session, "kata.set_metadata", map[string]any{"ref": ref, "patch": map[string]any{"work.attention": "ok"}})
	args := map[string]any{"ref": ref, "to": " reviewer/worker ", "message": "Please review", "teammate": "planner"}
	sent := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, true, sent["changed"])
	require.Equal(t, []any{"reviewer/worker"}, sent["recipients"])
	require.Equal(t, "example-agent", sent["event"].(map[string]any)["actor"])
	retried := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, false, retried["changed"])
	require.NotContains(t, retried, "event")
	require.Equal(t, sent["issue"].(map[string]any)["revision"], retried["issue"].(map[string]any)["revision"])
	inbox := callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})
	require.Len(t, inbox["requests"], 1)
	request := inbox["requests"].([]any)[0].(map[string]any)
	require.Equal(t, "example-agent", request["from"])
	require.Equal(t, "planner", request["teammate"])
	require.Equal(t, "Please review", request["message"])
	callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer", "message": "Separate actor inbox"})
	cleared := callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, true, cleared["changed"])
	require.Empty(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer/worker"})["requests"])
	require.Len(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})["requests"], 1)
	cleared = callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer/worker", "clear": true})
	require.Equal(t, false, cleared["changed"])
	shown := callCoordinationTool(t, session, "kata.show", map[string]any{"ref": ref})
	require.Equal(t, "ok", shown["issue"].(map[string]any)["metadata"].(map[string]any)["work.attention"])
}

// Contract: native linked requests carry authorized comment identities; typed
// replies clear the answering recipient and notify the requester unless that
// would overwrite an outstanding human request.
func TestMCPNotifyLinkedReplyAndHumanRequestProtection(t *testing.T) {
	for _, protected := range []bool{false, true} {
		t.Run(map[bool]string{false: "reply notice", true: "protected request"}[protected], func(t *testing.T) {
			session, _ := newCoordinationSession(t)
			created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Check the finding", "idempotency_key": "linked-create"})
			ref := created["issue"].(map[string]any)["ref"].(string)
			root := callCoordinationTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Inspect this contract", "teammate": "planner", "idempotency_key": "linked-root"})["comment"].(map[string]any)
			shown := callCoordinationTool(t, session, "kata.show", map[string]any{"ref": ref})
			root["handle"] = shown["issue"].(map[string]any)["comments"].([]any)[0].(map[string]any)["handle"]
			if protected {
				callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "example-agent/planner", "message": "A human decision is pending"})
			}
			callCoordinationTool(t, session, "kata.notify", map[string]any{"ref": ref, "to": "example-agent/worker", "message": "Please answer", "re": root["handle"], "teammate": "planner"})
			inbox := callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/worker"})
			require.Equal(t, root["uid"], inbox["requests"].([]any)[0].(map[string]any)["re"])
			reply := callCoordinationTool(t, session, "kata.comment", map[string]any{"ref": ref, "body": "Confirmed the notification contract against the isolated fixture.", "reply_to": root["handle"], "kind": "confirm", "teammate": "worker", "idempotency_key": "linked-answer"})["comment"].(map[string]any)
			require.Empty(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/worker"})["requests"])
			inbox = callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "example-agent/planner"})
			require.Len(t, inbox["requests"], 1)
			request := inbox["requests"].([]any)[0].(map[string]any)
			if protected {
				require.Equal(t, "A human decision is pending", request["message"])
				require.NotContains(t, request, "kind")
			} else {
				require.Equal(t, reply["uid"], request["re"])
				require.Equal(t, "confirm", request["kind"])
				require.Equal(t, "worker", request["teammate"])
			}
		})
	}
}

func TestMCPNotifyBroadcastUsesNativeAudienceAndRateLimit(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Coordinate review", "idempotency_key": "broadcast-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	callCoordinationTool(t, session, "kata.assign", map[string]any{"ref": ref, "owner": "reviewer"})
	args := map[string]any{"ref": ref, "broadcast": true, "message": "Please inspect the update"}
	sent := callCoordinationTool(t, session, "kata.notify", args)
	require.Equal(t, []any{"reviewer"}, sent["recipients"])
	inbox := callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})
	require.Equal(t, true, inbox["requests"].([]any)[0].(map[string]any)["broadcast"])
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.notify", Arguments: args})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, string(mustJSON(t, result)), "broadcast_rate_limited")
}

func TestMCPNotifyValidationAndScope(t *testing.T) {
	session, _ := newCoordinationSession(t)
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Keep untouched", "idempotency_key": "validation-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	for name, patch := range map[string]map[string]any{
		"blank recipient":             {"to": " "},
		"control recipient":           {"to": "review\ner"},
		"blank message":               {"message": " "},
		"long message":                {"message": strings.Repeat("é", 513)},
		"clear message":               {"clear": true},
		"clear linked":                {"clear": true, "message": nil, "re": "c:abcdef"},
		"clear broadcast":             {"clear": true, "message": nil, "broadcast": true},
		"to broadcast":                {"broadcast": true},
		"teammates without broadcast": {"teammates": true},
		"invalid teammate":            {"teammate": "bad/handle"},
		"unknown comment":             {"re": "c:abcdef"},
		"outside scope":               {"ref": "other-project#abcd"},
		"forged sender":               {"from": "someone-else"},
	} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"ref": ref, "to": "reviewer", "message": "Review"}
			for key, value := range patch {
				if value == nil {
					delete(args, key)
				} else {
					args[key] = value
				}
			}
			callToolError(t, session, "kata.notify", args)
		})
	}
	require.Empty(t, callCoordinationTool(t, session, "kata.inbox", map[string]any{"for": "reviewer"})["requests"])
	callCoordinationTool(t, session, "kata.close", map[string]any{"ref": ref, "reason": "done", "message": "Completed the isolated validation fixture checks.", "evidence": []any{map[string]any{"type": "test", "command": "fixture check"}}})
	callToolError(t, session, "kata.notify", map[string]any{"ref": ref, "to": "reviewer", "message": "Closed send"})
}

func TestMCPNotifyIdentityAndTeammateDefaults(t *testing.T) {
	env := testenv.New(t, testenv.WithRequireTokenIdentity())
	project, err := env.DB.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, _, err = env.DB.CreateAPIToken(t.Context(), db.CreateAPITokenParams{PlaintextToken: "fixture-token", Actor: "bound-actor", AdminActor: "fixture-admin"})
	require.NoError(t, err)
	client, err := kataclient.NewWithHTTPClient(env.URL, env.HTTP, kataclient.WithRequestEditor(func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer fixture-token")
		return nil
	}))
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: project.ID, ProjectName: project.Name, Actor: "bound-actor", Teammate: "default-worker", Version: "test"})
	created := callCoordinationTool(t, session, "kata.create", map[string]any{"title": "Attribute request", "idempotency_key": "identity-create"})
	ref := created["issue"].(map[string]any)["ref"].(string)
	for _, tc := range []struct {
		name     string
		teammate *string
		want     string
	}{
		{"startup", nil, "default-worker"}, {"override", new("reviewer"), "reviewer"}, {"suppressed", new(""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"ref": ref, "to": "recipient", "message": tc.name}
			if tc.teammate != nil {
				args["teammate"] = *tc.teammate
			}
			callCoordinationTool(t, session, "kata.notify", args)
			shown := callCoordinationTool(t, session, "kata.show", map[string]any{"ref": ref})
			raw := shown["issue"].(map[string]any)["metadata"].(map[string]any)[notification.MetadataKey("recipient")]
			var value notification.Value
			require.NoError(t, json.Unmarshal(mustJSON(t, raw), &value))
			require.Equal(t, "bound-actor", value.From)
			require.Equal(t, tc.want, value.Teammate)
		})
	}
	// Token identity overrides the request actor, including a mismatched startup.
	forged := connectTestServerWithOptions(t, Options{Client: client, ProjectID: project.ID, ProjectName: project.Name, Actor: "forged-actor", Version: "test"})
	sent := callCoordinationTool(t, forged, "kata.notify", map[string]any{"ref": ref, "to": "recipient", "message": "Keep token attribution"})
	require.Equal(t, "bound-actor", sent["event"].(map[string]any)["actor"])
	shown := callCoordinationTool(t, forged, "kata.show", map[string]any{"ref": ref})
	value := shown["issue"].(map[string]any)["metadata"].(map[string]any)[notification.MetadataKey("recipient")].(map[string]any)
	require.Equal(t, "bound-actor", value["from"])
}
