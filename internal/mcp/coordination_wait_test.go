package mcpserver

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
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
		default:
			assert.Equal(t, http.MethodGet, r.Method, "waiting must never consume or modify a request")
			writeJSON(w, map[string]any{"issue": snapshot(r), "labels": []any{}, "links": []any{}, "comments": []any{}})
		}
	})
	return connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "startup-actor", Teammate: "worker", Version: "test"})
}

func TestMCPCoordinationWaitAttentionPredicates(t *testing.T) {
	for _, tc := range []struct {
		name         string
		attention    any
		want, reason string
	}{
		{"attention", "stuck", "matched", "attention"},
		{"nonstring", 3, "timeout", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := coordinationWaitSession(t, func(_ *http.Request) map[string]any {
				issue := issueJSON(1, "spoke-project", "abc1")
				issue["metadata"] = map[string]any{"work.attention": tc.attention, "work.attention_msg": "Need a decision"}
				return issue
			})
			out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1"}, "until": "attention", "timeout_seconds": 1})
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
		name, actor, teammate, effective string
		frames                           []map[string]any
		explicit                         bool
	}{
		{"changed", "startup-actor", "worker", "bound-actor", []map[string]any{{key: map[string]any{"from": "reviewer", "message": "old"}}, {key: map[string]any{"from": "reviewer", "message": "new"}}}, true},
		{"null and exact recipient", "startup-actor", "worker", "bound-actor", []map[string]any{{}, {key: nil, other: map[string]any{"from": "reviewer", "message": "wrong inbox"}}, {key: map[string]any{"from": "reviewer", "message": "answer"}}}, false},
		{"normalized default identity", "worker ", "review", "", []map[string]any{{}, {notification.MetadataKey("worker/review"): map[string]any{"from": "reviewer", "message": "answer"}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var shows, lookups atomic.Int64
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/notifications/actor":
					lookups.Add(1)
					effective := tc.effective
					if effective == "" {
						effective = r.URL.Query().Get("requested_actor")
					}
					writeJSON(w, map[string]any{"effective_actor": effective})
				default:
					issue := issueJSON(1, "spoke-project", "abc1")
					index := min(int(shows.Add(1))-1, len(tc.frames)-1)
					issue["metadata"] = tc.frames[index]
					writeJSON(w, map[string]any{"issue": issue})
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: tc.actor, Teammate: tc.teammate, Version: "test"})
			args := map[string]any{"refs": []string{"abc1"}, "until": "reply", "timeout_seconds": 2}
			if tc.explicit {
				args["for"] = " bound-actor/worker "
			}
			out := callAdministrationTool(t, session, "kata.wait", args)
			require.Equal(t, "matched", out["reason"])
			require.Equal(t, int64(len(tc.frames)), shows.Load(), "the first snapshot and unrelated slots cannot wake the wait")
			state := out["states"].([]any)[0].(map[string]any)
			require.Equal(t, true, state["matched"])
			require.Equal(t, "reply", state["reason"])
			if tc.explicit {
				require.Zero(t, lookups.Load())
			}
		})
	}
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
	out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "until": "attention", "timeout_seconds": 2})
	require.Equal(t, "matched", out["reason"])
	for _, raw := range out["states"].([]any) {
		require.Equal(t, true, raw.(map[string]any)["matched"])
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
	for i, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"refs": []string{"abc1"}, "until": "attention", "status": "closed"}, "not: validated against"},
		{map[string]any{"refs": []string{"abc1"}, "until": "unknown"}, "enum"},
		{map[string]any{"refs": []string{"abc1"}, "status": "unknown"}, "enum"},
		{map[string]any{"refs": []string{"abc1"}, "until": "attention", "for": "worker"}, "for requires until=reply"},
		{map[string]any{"refs": []string{"abc1"}, "until": "reply", "for": " "}, "recipient"},
		{map[string]any{"refs": []string{"abc1"}, "until": "attention", "timeout_seconds": 301}, "maximum"},
		{map[string]any{"refs": []string{"abc1"}, "until": "attention", "timeout_seconds": -1}, "minimum"},
		{map[string]any{"refs": []string{"other-project#abc1"}, "until": "attention"}, "outside"},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.want, i), func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: tc.args})
			if err != nil {
				require.Contains(t, err.Error(), tc.want)
				return
			}
			require.True(t, result.IsError)
			require.Contains(t, string(mustJSON(t, result.Content)), tc.want)
		})
	}
}

func TestMCPCoordinationWaitCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, path, until string
		deadline          bool
	}{
		{"issue cancellation", "/api/v1/projects/1/issues/abc1", "attention", false},
		{"actor cancellation", "/api/v1/projects/1/notifications/actor", "reply", false},
		{"actor deadline", "/api/v1/projects/1/notifications/actor", "reply", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, cancelled := make(chan struct{}, 1), make(chan struct{})
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					entered <- struct{}{}
					<-r.Context().Done()
					close(cancelled)
					return
				}
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/issues/abc1":
					writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "abc1")})
				default:
					http.NotFound(w, r)
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			timeout := 5
			if tc.deadline {
				timeout = 1
			}
			type result struct {
				out *sdkmcp.CallToolResult
				err error
			}
			done := make(chan result, 1)
			go func() {
				out, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{"abc1"}, "until": tc.until, "timeout_seconds": timeout}})
				done <- result{out, err}
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("wait did not reach request")
			}
			if !tc.deadline {
				cancel()
			}
			select {
			case got := <-done:
				if tc.deadline {
					require.NoError(t, got.err)
					require.Contains(t, string(mustJSON(t, got.out.StructuredContent)), "timeout")
				} else {
					require.ErrorIs(t, got.err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("wait did not stop")
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("daemon request was not cancelled")
			}
		})
	}
}

func TestMCPCoordinationWaitReplyResolvesActorPerProject(t *testing.T) {
	actors := map[string]string{"/api/v1/projects/1/notifications/actor": "first-bound-actor", "/api/v1/projects/2/notifications/actor": "second-bound-actor"}
	lookups := map[int64]*atomic.Int64{1: new(atomic.Int64), 2: new(atomic.Int64)}
	shows := map[string]*atomic.Int64{"abc1": new(atomic.Int64), "def2": new(atomic.Int64)}
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "first-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "second-project"),
			}})
		case "/api/v1/projects/1/notifications/actor", "/api/v1/projects/2/notifications/actor":
			projectID := int64(1)
			if strings.Contains(r.URL.Path, "/2/") {
				projectID = 2
			}
			lookups[projectID].Add(1)
			writeJSON(w, map[string]any{"effective_actor": actors[r.URL.Path]})
		case "/api/v1/projects/1/issues/abc1", "/api/v1/projects/2/issues/def2":
			projectID, name, ref, actor := int64(1), "first-project", "abc1", "first-bound-actor"
			if strings.Contains(r.URL.Path, "/2/") {
				projectID, name, ref, actor = 2, "second-project", "def2", "second-bound-actor"
			}
			message := "before"
			if shows[ref].Add(1) > 1 {
				message = "reply arrived"
			}
			issue := issueJSON(projectID, name, ref)
			if projectID == 2 {
				issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
			}
			issue["metadata"] = map[string]any{notification.MetadataKey(actor + "/worker"): map[string]any{"from": "reviewer", "message": message}}
			writeJSON(w, map[string]any{"issue": issue, "labels": []any{}, "links": []any{}, "comments": []any{}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "first-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "second-project"},
	})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "startup-actor", Teammate: "worker", Version: "test"})
	out := callAdministrationTool(t, session, "kata.wait", map[string]any{
		"refs": []string{"first-project#abc1", "second-project#def2"}, "until": "reply", "timeout_seconds": 3,
	})
	require.Equal(t, "matched", out["reason"])
	require.GreaterOrEqual(t, lookups[1].Load(), int64(1))
	require.GreaterOrEqual(t, lookups[2].Load(), int64(1))
	require.GreaterOrEqual(t, shows["abc1"].Load(), int64(2))
	require.GreaterOrEqual(t, shows["def2"].Load(), int64(2))
}

func TestMCPCoordinationWaitReplySurvivesProjectMove(t *testing.T) {
	var shows atomic.Int64
	client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			writeJSON(w, map[string]any{"projects": []any{
				projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "first-project"),
				projectJSON(2, "01HBBBBBBBBBBBBBBBBBBBBBBB", "second-project"),
			}})
		case "/api/v1/projects/1/notifications/actor":
			writeJSON(w, map[string]any{"effective_actor": "worker"})
		case "/api/v1/projects/2/notifications/actor":
			writeJSON(w, map[string]any{"effective_actor": "other-worker"})
		case "/api/v1/projects/1/issues/abc1":
			issue := issueJSON(1, "first-project", "abc1")
			message := "before"
			if shows.Add(1) > 1 {
				issue["project_id"] = 2
				issue["project_uid"] = "01HBBBBBBBBBBBBBBBBBBBBBBB"
				message = "reply arrived"
			}
			issue["metadata"] = map[string]any{
				notification.MetadataKey("worker"):       map[string]any{"from": "reviewer", "message": message},
				notification.MetadataKey("other-worker"): map[string]any{"from": "reviewer", "message": "before"},
			}
			writeJSON(w, map[string]any{"issue": issue, "labels": []any{}, "links": []any{}, "comments": []any{}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	scope, err := NewAllowlistScope([]ProjectIdentity{
		{ID: 1, UID: "01HAAAAAAAAAAAAAAAAAAAAAAA", Name: "first-project"},
		{ID: 2, UID: "01HBBBBBBBBBBBBBBBBBBBBBBB", Name: "second-project"},
	})
	require.NoError(t, err)
	session := connectTestServerWithOptions(t, Options{Client: client, Scope: scope, Actor: "worker", Version: "test"})
	args := map[string]any{"refs": []string{"first-project#abc1"}, "until": "reply", "timeout_seconds": 2}
	out := callAdministrationTool(t, session, "kata.wait", args)
	require.Equal(t, "matched", out["reason"])
}

func TestMCPCoordinationWaitActorLookupNotFound(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprintf("structured=%t", structured), func(t *testing.T) {
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/issues/abc1":
					writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "abc1")})
				case "/api/v1/projects/1/notifications/actor":
					if structured {
						w.WriteHeader(http.StatusNotFound)
						writeJSON(w, map[string]any{"status": http.StatusNotFound, "error": map[string]any{"code": "not_found", "message": "resource not found"}})
					} else {
						http.NotFound(w, r)
					}
				default:
					http.NotFound(w, r)
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "startup-actor", Version: "test"})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{"abc1"}, "until": "reply", "timeout_seconds": 2}})
			require.NoError(t, err)
			require.True(t, result.IsError)
			text := string(mustJSON(t, result))
			if structured {
				require.Contains(t, text, "resource not found")
				require.NotContains(t, text, "explicit for")
			} else {
				require.Contains(t, text, "explicit for")
			}
		})
	}
}

func TestMCPCoordinationWaitStatusAnySnapshot(t *testing.T) {
	session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
		ref := "abc1"
		if strings.HasSuffix(r.URL.Path, "/def1") {
			ref = "def1"
		}
		return issueJSON(1, "spoke-project", ref)
	})
	out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "status": "open", "any": true, "timeout_seconds": 1})
	require.Equal(t, "matched", out["reason"])
	require.Len(t, out["states"], 2)
}

func TestMCPCoordinationWaitStatusPollsPastStalledPeer(t *testing.T) {
	t.Run("all eventually match", func(t *testing.T) {
		var reads atomic.Int64
		session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
			ref := "abc1"
			if strings.HasSuffix(r.URL.Path, "/def1") {
				ref = "def1"
			}
			issue := issueJSON(1, "spoke-project", ref)
			if reads.Add(1) > 2 {
				issue["status"] = "closed"
			}
			return issue
		})
		out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "status": "closed", "timeout_seconds": 2})
		require.Equal(t, "matched", out["reason"])
		require.Len(t, out["states"], 2)
	})
	for _, mode := range []string{"status", "until"} {
		for _, anyMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/any=%t", mode, anyMode), func(t *testing.T) {
				var reads atomic.Int64
				session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
					if strings.HasSuffix(r.URL.Path, "/abc1") {
						<-r.Context().Done()
						return nil
					}
					issue := issueJSON(1, "spoke-project", "def1")
					if reads.Add(1) > 1 {
						if mode == "status" {
							issue["status"] = "closed"
						} else {
							issue["metadata"] = map[string]any{"work.attention": "stuck"}
						}
					}
					return issue
				})
				value := "closed"
				if mode == "until" {
					value = "attention"
				}
				out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, mode: value, "any": anyMode, "timeout_seconds": 1})
				if anyMode {
					require.Equal(t, "matched", out["reason"])
				} else {
					require.Equal(t, "timeout", out["reason"])
				}
				require.GreaterOrEqual(t, reads.Load(), int64(2))
				require.Equal(t, true, out["states"].([]any)[0].(map[string]any)["matched"])
			})
		}
	}
	for _, nonmatchFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonmatchFirst=%t", nonmatchFirst), func(t *testing.T) {
			var aReads, bReads atomic.Int64
			aSecond := make(chan struct{})
			bSecond := make(chan struct{})
			session := coordinationWaitSession(t, func(r *http.Request) map[string]any {
				if strings.HasSuffix(r.URL.Path, "/abc1") {
					issue := issueJSON(1, "spoke-project", "abc1")
					if aReads.Add(1) == 1 {
						if nonmatchFirst {
							<-bSecond
						}
						issue["status"] = "closed"
						return issue
					}
					close(aSecond)
					<-r.Context().Done()
					return nil
				}
				issue := issueJSON(1, "spoke-project", "def1")
				bRead := bReads.Add(1)
				if bRead == 1 {
					if !nonmatchFirst {
						<-aSecond
					}
					return issue
				}
				if nonmatchFirst && bRead == 2 {
					close(bSecond)
					<-aSecond
				}
				issue["status"] = "closed"
				return issue
			})
			out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{"abc1", "def1"}, "status": "closed", "timeout_seconds": 2})
			require.Equal(t, "timeout", out["reason"])
			require.GreaterOrEqual(t, bReads.Load(), int64(2))
		})
	}
}

func TestMCPCoordinationWaitAnyWithMissingIssue(t *testing.T) {
	for _, tc := range []struct {
		name, field, value, ref string
		unavailable             bool
	}{
		{"attention", "until", "attention", "abc1", false}, {"out of scope", "until", "attention", "other-project#abc1", false}, {"retry limit", "until", "attention", "abc1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int64
			var failures atomic.Int64
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/issues/abc1":
					if tc.unavailable {
						failures.Add(1)
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					http.NotFound(w, r)
				case "/api/v1/projects/1/issues/def1":
					issue := issueJSON(1, "spoke-project", "def1")
					if (!tc.unavailable || failures.Load() >= 3) && reads.Add(1) > 1 {
						issue["metadata"] = map[string]any{"work.attention": "stuck"}
					}
					writeJSON(w, map[string]any{"issue": issue})
				default:
					http.NotFound(w, r)
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			args := map[string]any{"refs": []string{tc.ref, "def1"}, tc.field: tc.value, "any": true, "timeout_seconds": 2}

			out := callAdministrationTool(t, session, "kata.wait", args)
			require.Equal(t, "matched", out["reason"])
			if tc.unavailable {
				require.Equal(t, int64(3), failures.Load())
			}
		})
	}
}

func TestMCPCoordinationWaitAnyReportsInitialInputErrorAtDeadline(t *testing.T) {
	for _, tc := range []struct {
		name, ref, want string
		transient       bool
	}{
		{"missing", "abc1", "404", false},
		{"out of scope", "other-project#abc1", "outside", false},
		{"transient", "abc1", "timeout", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/issues/abc1":
					if tc.transient {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					http.NotFound(w, r)
				default:
					writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "def1")})
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{tc.ref, "def1"}, "until": "attention", "any": true, "timeout_seconds": 1}})
			require.NoError(t, err)
			if tc.transient {
				require.False(t, result.IsError)
				require.Equal(t, tc.want, result.StructuredContent.(map[string]any)["reason"])
			} else {
				require.True(t, result.IsError)
				require.Contains(t, string(mustJSON(t, result.Content)), tc.want)
			}
		})
	}
}

func TestMCPCoordinationWaitDeletedIssue(t *testing.T) {
	session, store := newCoordinationSession(t)
	created := callAdministrationTool(t, session, "kata.create", map[string]any{"title": "Deleted attention", "idempotency_key": "deleted-attention"})
	summary := created["issue"].(map[string]any)
	issue, err := store.IssueByUID(t.Context(), summary["uid"].(string), db.IncludeDeletedNo)
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"work.attention": jsontext.Value(`"stuck"`)}})
	require.NoError(t, err)
	_, _, _, err = store.SoftDeleteIssue(t.Context(), issue.ID, "worker")
	require.NoError(t, err)
	ref := summary["ref"].(string)
	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{ref}, "until": "attention", "timeout_seconds": 1}})
	require.NoError(t, err)
	require.True(t, result.IsError, string(mustJSON(t, result.Content)))
	require.Contains(t, string(mustJSON(t, result.Content)), "404")
	out := callAdministrationTool(t, session, "kata.wait", map[string]any{"refs": []string{ref}, "status": "deleted", "timeout_seconds": 1})
	require.Equal(t, "matched", out["reason"])
}

func TestMCPCoordinationWaitStatusReturnsMissing(t *testing.T) {
	for _, anyMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("any=%t", anyMode), func(t *testing.T) {
			peerStarted := make(chan struct{})
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/projects":
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
				case "/api/v1/projects/1/issues/abc1":
					select {
					case <-peerStarted:
					case <-r.Context().Done():
						return
					}
					http.NotFound(w, r)
				default:
					close(peerStarted)
					if anyMode {
						writeJSON(w, map[string]any{"issue": issueJSON(1, "spoke-project", "def1")})
						return
					}
					<-r.Context().Done()
				}
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": []string{"abc1", "def1"}, "status": "open", "any": anyMode, "timeout_seconds": 1}})
			require.NoError(t, err)
			require.True(t, result.IsError, string(mustJSON(t, result.Content)))
			require.Contains(t, string(mustJSON(t, result.Content)), "404")
		})
	}
}

func TestMCPCoordinationWaitUntilRetriesTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name, until string
		statuses    []int
	}{
		{"reply baseline", "reply", []int{200, 503, 200}},
		{"successful read resets failures", "attention", []int{503, 503, 200, 503, 503, 200}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int64
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
					return
				}
				i := min(int(reads.Add(1))-1, len(tc.statuses)-1)
				if tc.statuses[i] != http.StatusOK {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				issue := issueJSON(1, "spoke-project", "abc1")
				if tc.until == "reply" {
					message := "before"
					if i == len(tc.statuses)-1 {
						message = "after"
					}
					issue["metadata"] = map[string]any{notification.MetadataKey("worker"): map[string]any{"message": message}}
				} else if i == len(tc.statuses)-1 {
					issue["metadata"] = map[string]any{"work.attention": "stuck"}
				}
				writeJSON(w, map[string]any{"issue": issue})
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			args := map[string]any{"refs": []string{"abc1"}, "until": tc.until, "timeout_seconds": 3}
			if tc.until == "reply" {
				args["for"] = "worker"
			}
			out := callAdministrationTool(t, session, "kata.wait", args)
			require.Equal(t, "matched", out["reason"])
			require.Equal(t, tc.until, out["states"].([]any)[0].(map[string]any)["reason"])
			require.Equal(t, int64(len(tc.statuses)), reads.Load())
		})
	}
}

func TestMCPCoordinationWaitUntilPersistentFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		any    bool
		refs   []string
		reads  int64
	}{
		{"transient all", 503, false, []string{"abc1"}, 3},
		{"transient any", 503, true, []string{"abc1"}, 3},
		{"missing any", 404, true, []string{"abc1", "def1"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int64
			client := reviewClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/projects" {
					writeJSON(w, map[string]any{"projects": []any{projectJSON(1, "01HAAAAAAAAAAAAAAAAAAAAAAA", "spoke-project")}})
					return
				}
				reads.Add(1)
				http.Error(w, "unavailable", tc.status)
			})
			session := connectTestServerWithOptions(t, Options{Client: client, ProjectID: 1, ProjectName: "spoke-project", Actor: "worker", Version: "test"})
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "kata.wait", Arguments: map[string]any{"refs": tc.refs, "until": "attention", "any": tc.any, "timeout_seconds": 2}})
			require.NoError(t, err)
			require.True(t, result.IsError)
			require.Contains(t, string(mustJSON(t, result.Content)), fmt.Sprint(tc.status))
			require.Equal(t, tc.reads, reads.Load())
		})
	}
}
