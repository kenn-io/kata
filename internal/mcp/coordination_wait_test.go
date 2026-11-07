package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/notification"
)

// The HTTP fixtures control transitions; callers still use the public MCP
// schema and real generated client. There are no sleeps driving mutations.
func coordinationWaitSession(t *testing.T, snapshot func(*http.Request) map[string]any) *sdkmcp.ClientSession {
	t.Helper()
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/instance":
			writeJSON(w, map[string]any{"instance_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "version": "test", "auth": map[string]any{"kind": "token", "actor": "bound-actor"}})
		default:
			require.Equal(t, http.MethodGet, r.Method, "waiting must never consume or modify a request")
			writeJSON(w, map[string]any{"issue": snapshot(r), "labels": []any{}, "links": []any{}, "comments": []any{}})
		}
	})
	return connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "startup-actor", Teammate: "worker", Version: "test"})
}

func TestMCPCoordinationWaitAttentionPredicates(t *testing.T) {
	for _, tc := range []struct {
		name, until, status string
		attention           any
		want, reason        string
	}{
		{"needs human", "attention", "open", "needs-human", "matched", "attention"},
		{"custom attention", "attention", "open", "waiting-on-review", "matched", "attention"},
		{"stuck", "stuck", "open", "stuck", "matched", "attention"},
		{"exact needs human", "needs-human", "open", "needs-human", "matched", "attention"},
		{"wrong level", "needs-human", "open", "stuck", "timeout", ""},
		{"ok", "attention", "open", "ok", "timeout", ""},
		{"nonstring", "attention", "open", 3, "timeout", ""},
		{"unset", "attention", "open", nil, "timeout", ""},
		{"closed precedence", "attention", "closed", "stuck", "matched", "closed"},
		{"closed reply", "reply", "closed", nil, "matched", "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := coordinationWaitSession(t, func(_ *http.Request) map[string]any {
				issue := issueJSON(1, "spoke-project", "abc1")
				issue["status"] = tc.status
				issue["metadata"] = map[string]any{"work.attention": tc.attention, "work.attention_msg": "Need a decision", notification.MetadataKey("bound-actor/worker"): map[string]any{"from": "reviewer", "message": "A separate request"}}
				return issue
			})
			out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1"}, "until": tc.until, "timeout_seconds": 1})
			require.Equal(t, tc.want, out["reason"])
			state := out["states"].([]any)[0].(map[string]any)
			require.Equal(t, tc.want == "matched", state["matched"])
			if tc.reason != "" {
				require.Equal(t, tc.reason, state["reason"])
			}
			if tc.reason == "attention" {
				require.Equal(t, tc.attention, state["attention"])
				require.Equal(t, "Need a decision", state["attention_msg"])
			} else {
				require.NotContains(t, state, "attention")
			}
		})
	}
}

func TestMCPCoordinationWaitReplyBaseline(t *testing.T) {
	key := notification.MetadataKey("bound-actor/worker")
	other := notification.MetadataKey("bound-actor")
	for _, tc := range []struct {
		name     string
		frames   []map[string]any
		explicit bool
	}{
		{"changed", []map[string]any{{key: map[string]any{"from": "reviewer", "message": "old"}}, {key: map[string]any{"from": "reviewer", "message": "new"}}}, true},
		{"clear", []map[string]any{{key: map[string]any{"from": "reviewer", "message": "old"}}, {}}, true},
		{"reorder", []map[string]any{{key: json.RawMessage(`{"message":"old","from":"reviewer"}`)}, {key: json.RawMessage(`{"from":"reviewer","message":"old"}`)}, {key: map[string]any{"from": "reviewer", "message": "new"}}}, true},
		{"null and exact recipient", []map[string]any{{}, {key: nil, other: map[string]any{"from": "reviewer", "message": "wrong inbox"}}, {key: map[string]any{"from": "reviewer", "message": "answer"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var shows atomic.Int64
			session := coordinationWaitSession(t, func(_ *http.Request) map[string]any {
				issue := issueJSON(1, "spoke-project", "abc1")
				index := min(int(shows.Add(1))-1, len(tc.frames)-1)
				issue["metadata"] = tc.frames[index]
				return issue
			})
			args := map[string]any{"refs": []string{"abc1"}, "until": "reply", "timeout_seconds": 2}
			if tc.explicit {
				args["for"] = " bound-actor/worker "
			}
			out := callCoordinationTool(t, session, "kata.wait", args)
			require.Equal(t, "matched", out["reason"])
			require.Equal(t, int64(len(tc.frames)), shows.Load(), "the first snapshot and semantically identical slots cannot wake the wait")
			state := out["states"].([]any)[0].(map[string]any)
			require.Equal(t, true, state["matched"])
			require.Equal(t, "reply", state["reason"])
		})
	}
}

// Contract: the inferred inbox uses the same normalized identity as native
// reply delivery, including accepted whitespace around the startup actor.
func TestMCPCoordinationWaitReplyNormalizesDefaultIdentity(t *testing.T) {
	var shows atomic.Int64
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
		case "/api/v1/instance":
			writeJSON(w, map[string]any{"instance_uid": "01HAAAAAAAAAAAAAAAAAAAAAAA", "auth": map[string]any{"kind": "token"}})
		default:
			issue := issueJSON(1, "spoke-project", "abc1")
			if shows.Add(1) > 1 {
				issue["metadata"] = map[string]any{notification.MetadataKey("worker/review"): map[string]any{"from": "reviewer", "message": "An answer arrived"}}
			}
			writeJSON(w, map[string]any{"issue": issue})
		}
	})
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker ", Teammate: "review", Version: "test"})
	out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1"}, "until": "reply", "timeout_seconds": 1})
	require.Equal(t, "matched", out["reason"], "native replies are delivered to the canonical actor/teammate slot")
	require.Equal(t, "reply", out["states"].([]any)[0].(map[string]any)["reason"])
}

func TestMCPCoordinationWaitAnyStopsBeforeLaterRef(t *testing.T) {
	var later atomic.Bool
	session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
		if strings.HasSuffix(r.URL.Path, "/def1") {
			later.Store(true)
		}
		issue := issueJSON(1, "spoke-project", "abc1")
		issue["metadata"] = map[string]any{"work.attention": "stuck"}
		return issue
	})
	out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "until": "attention", "any": true, "timeout_seconds": 1})
	require.Equal(t, "matched", out["reason"])
	require.False(t, later.Load(), "a slow later reference must not postpone a satisfied any join")
}

func TestMCPCoordinationWaitAllRetainsObservedCompletions(t *testing.T) {
	var first, second atomic.Int64
	session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
		issue := issueJSON(1, "spoke-project", "abc1")
		attention := "ok"
		if strings.HasSuffix(r.URL.Path, "/abc1") {
			if first.Add(1) == 1 {
				attention = "stuck"
			}
		} else {
			issue["short_id"], issue["uid"] = "def1", "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			if second.Add(1) > 1 {
				attention = "needs-human"
			}
		}
		issue["metadata"] = map[string]any{"work.attention": attention}
		return issue
	})
	out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "until": "attention", "timeout_seconds": 2})
	require.Equal(t, "matched", out["reason"])
	for _, raw := range out["states"].([]any) {
		require.Equal(t, true, raw.(map[string]any)["matched"])
	}
}

// Contract: an until completion remains visible after a later member uses up
// the deadline, including when that completion arrived after a full round.
func TestMCPCoordinationWaitTimeoutRetainsObservedCompletions(t *testing.T) {
	for _, laterRound := range []bool{false, true} {
		t.Run(map[bool]string{false: "first round", true: "later round"}[laterRound], func(t *testing.T) {
			var first, second atomic.Int64
			session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
				issue := issueJSON(1, "spoke-project", "abc1")
				if strings.HasSuffix(r.URL.Path, "/abc1") {
					if first.Add(1) > 1 || !laterRound {
						issue["metadata"] = map[string]any{"work.attention": "stuck"}
					}
				} else {
					issue["short_id"], issue["uid"] = "def1", "01ARZ3NDEKTSV4RRFFQ69G5FAW"
					if second.Add(1) > 1 || !laterRound {
						<-r.Context().Done()
					}
				}
				return issue
			})
			out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "until": "attention", "timeout_seconds": 1})
			require.Equal(t, "timeout", out["reason"])
			states := out["states"].([]any)
			require.NotEmpty(t, states, "an observed completion must survive a later stalled read")
			state := states[0].(map[string]any)
			require.Equal(t, "spoke-project#abc1", state["ref"])
			require.Equal(t, true, state["matched"])
			require.Equal(t, "attention", state["reason"])
			require.Equal(t, "stuck", state["attention"])
		})
	}
}

func TestMCPCoordinationWaitRejectsReusedIssueIdentity(t *testing.T) {
	var shows atomic.Int64
	session := coordinationWaitSession(t, func(_ *http.Request) map[string]any {
		issue := issueJSON(1, "spoke-project", "abc1")
		if shows.Add(1) > 1 {
			issue["uid"] = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
			issue["metadata"] = map[string]any{notification.MetadataKey("bound-actor/worker"): "unrelated"}
		}
		return issue
	})
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{"abc1"}, "until": "reply", "for": "bound-actor/worker", "timeout_seconds": 2}})
	require.NoError(t, err)
	require.True(t, result.IsError)
	require.Contains(t, string(mustJSON(t, result)), "identity changed")
}

func TestMCPCoordinationWaitValidation(t *testing.T) {
	session := coordinationWaitSession(t, func(_ *http.Request) map[string]any { return issueJSON(1, "spoke-project", "abc1") })
	for _, args := range []map[string]any{
		{"refs": []string{"abc1"}, "until": "attention", "status": "closed"},
		{"refs": []string{"abc1"}, "until": "unknown"},
		{"refs": []string{"abc1"}, "until": "attention", "for": "worker"},
		{"refs": []string{"abc1"}, "until": "reply", "for": " "},
		{"refs": []string{"abc1"}, "until": "attention", "timeout_seconds": 301},
		{"refs": []string{"other-project#abc1"}, "until": "attention"},
	} {
		callToolError(t, session, "kata.wait", args)
	}
}

func TestMCPCoordinationWaitDeadlineCoversReplyIdentityDiscovery(t *testing.T) {
	var reached atomic.Bool
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			reached.Store(true)
			<-r.Context().Done()
			return
		}
		writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
	})
	session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
	start := time.Now()
	out := callCoordinationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1"}, "until": "reply", "timeout_seconds": 1})
	require.True(t, reached.Load())
	require.Equal(t, "timeout", out["reason"])
	require.Less(t, time.Since(start), 3*time.Second)
}

func TestMCPCoordinationWaitParentCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{})
	session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
		entered <- struct{}{}
		<-r.Context().Done()
		close(cancelled)
		return issueJSON(1, "spoke-project", "abc1")
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{"abc1"}, "until": "attention", "timeout_seconds": 5}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("wait never reached the issue lookup")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop the wait")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("daemon request was not cancelled")
	}
}
