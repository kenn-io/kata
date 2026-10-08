package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestWaitReplyInitialSlotIsBaseline(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	t.Setenv("KATA_INBOX_USER", "reader")
	issue, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Existing reply", Author: "worker"})
	require.NoError(t, e)
	_, e = env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"notify.cmVhZGVy": jsontext.Value(`{"from":"worker","message":"old","re":"old"}`)}})
	require.NoError(t, e)
	_, _, e = runCLIWithErr(t, env, dir, "wait", issue.ShortID, "--until", "reply", "--poll-interval", waitFastPoll, "--timeout", "200ms")
	_ = requireCLIError(t, e, ExitWaitTimeout)
}

func TestWaitReplySlotTransition(t *testing.T) {
	run := waitRun{mode: waitMode("reply"), start: time.Now()}
	target := &waitTarget{arg: "example", state: targetPending}
	old := issueState{status: "open", reply: jsontext.Value(`{"message":"old","re":"finding"}`)}
	require.False(t, evalTarget(run, target, old, ""))
	same := issueState{status: "open", reply: jsontext.Value(`{ "re": "finding", "message": "old" }`)}
	require.False(t, evalTarget(run, target, same, ""), "formatting is not a new request")
	require.True(t, evalTarget(run, target, issueState{status: "open"}, ""), "clearing a slot is a transition")
	require.Equal(t, "reply", target.result.Reason)
	target = &waitTarget{state: targetPending}
	require.False(t, evalTarget(run, target, issueState{status: "open"}, ""))
	require.True(t, evalTarget(run, target, old, ""))
}

func FuzzWaitReplyFirstPollBaseline(f *testing.F) {
	f.Add("old", "new")
	f.Fuzz(func(t *testing.T, old, next string) {
		if len(old)+len(next) > 2048 {
			return
		}
		run := waitRun{mode: waitMode("reply"), start: time.Now()}
		target := &waitTarget{state: targetPending}
		rawOld, e := json.Marshal(old)
		if e != nil {
			return
		}
		rawNew, e := json.Marshal(next)
		if e != nil {
			return
		}
		before := issueState{status: "open", reply: rawOld}
		after := issueState{status: "open", reply: rawNew}
		require.False(t, evalTarget(run, target, before, ""))
		require.Equal(t, old != next, evalTarget(run, target, after, ""))
	})
}

// The first successful poll establishes the baseline; a startup 503 and a
// mutation of another inbox must not satisfy a reply wait.
func TestWaitReplyTransientBaselineAndRecipientIsolation(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/projects/resolve":
			_, _ = w.Write([]byte(`{"project":{"id":1,"name":"example-project"}}`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/projects/1/issues/"):
			n := polls.Add(1)
			if n == 1 {
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"restarting"}}`))
				return
			}
			slot := `{"from":"worker","message":"old","re":"finding"}`
			if n >= 4 {
				slot = `{"from":"worker","message":"new","re":"reply"}`
			}
			_, _ = fmt.Fprintf(w, `{"issue":{"short_id":"abcd","status":"open","metadata":{"notify.cmVhZGVy":%s,"notify.b3RoZXI":{"message":"poll %d"}}}}`, slot, n)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	resetFlags(t)
	t.Setenv("KATA_INBOX_USER", "reader")
	stdout, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), server.URL), "--json", "wait", "example-project#abcd", "--until", "reply", "--timeout", "5s", "--poll-interval", "25ms")
	require.NoError(t, err)
	require.GreaterOrEqual(t, polls.Load(), int32(4))
	out := parseWaitJSON(t, stdout)
	require.Len(t, out.Results, 1)
	require.Equal(t, "reply", out.Results[0].Reason)
}

func TestWaitReplyJoinAndClosedCompletion(t *testing.T) {
	for _, join := range []waitJoin{joinAny, joinAll} {
		t.Run(fmt.Sprint(join), func(t *testing.T) {
			run := waitRun{mode: waitReply, join: join, start: time.Now()}
			targets := []*waitTarget{{state: targetPending}, {state: targetPending}}
			for _, target := range targets {
				require.False(t, evalTarget(run, target, issueState{status: "open"}, ""))
			}
			require.True(t, evalTarget(run, targets[0], issueState{status: "open", reply: jsontext.Value(`{"message":"reply"}`)}, ""))
			require.Equal(t, join == joinAny, waitComplete(targets, join))
			require.True(t, evalTarget(run, targets[1], issueState{status: "closed"}, ""))
			require.Equal(t, "closed", targets[1].result.Reason)
			require.True(t, waitComplete(targets, join))
		})
	}
}
