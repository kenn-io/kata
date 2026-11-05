package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// Contract: one ask can park its issue, without changing other requests or its owner.
func TestNotifyFollowUpSharedScheduleAndOutput(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Await an answer")
	runCLI(t, env, dir, "notify", ref, "--to", "other-person", "--message", "Keep this ask")
	before, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	other := notificationMetadata(t, []byte(before.Metadata))[notificationMetadataKey("other-person")]
	out := runCLIAs(t, env, dir, "coordinator", "--agent", "notify", ref,
		"--to", "alex-outside", "--message", "Please decide", "--follow-up", "2099-11-10")
	require.Contains(t, out, "scheduled_on=2099-11-10")
	out = runCLIAs(t, env, dir, "coordinator", "--json", "notify", ref,
		"--to", "alex-outside", "--message", "Please decide", "--follow-up", "2099-11-10")
	var result struct {
		ScheduledOn string   `json:"scheduled_on"`
		Changed     bool     `json:"changed"`
		Issue       db.Issue `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.False(t, result.Changed)
	require.Equal(t, "2099-11-10", result.ScheduledOn)
	require.Contains(t, runCLI(t, env, dir, "inbox", "--for", "alex-outside"), "Please decide")
	require.NotContains(t, runCLI(t, env, dir, "ready", "--agent"), ref)
	runCLI(t, env, dir, "notify", ref, "--to", "alex-outside", "--clear")
	issue := fetchMetaIssueViaHTTP(t, env, pid, ref)
	metadata := notificationMetadata(t, issue.Issue.Metadata)
	require.Equal(t, `"2099-11-10"`, metadata["scheduled_on"])
	require.JSONEq(t, other, metadata[notificationMetadataKey("other-person")])
	after, err := env.DB.IssueByID(t.Context(), before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Owner, after.Owner)
	require.NotContains(t, metadata, notificationMetadataKey("alex-outside"))
	// Existing calls keep their output shape even when the issue has a date.
	out = runCLI(t, env, dir, "--agent", "notify", ref, "--to", "alex-outside", "--message", "Again")
	require.NotContains(t, out, "scheduled_on=")
}

func TestNotifyFollowUpRejectsInvalidDatesWithoutReplacingAsk(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Reject incomplete asks")
	runCLI(t, env, dir, "notify", ref, "--to", "alex", "--message", "Original", "--follow-up", "2099-11-10")
	before := fetchMetaIssueViaHTTP(t, env, pid, ref)
	for _, value := range []string{"", "-", "monday", "2099-02-30", "2099-11-10T09:00:00-07:00"} {
		_, err := runCLICapture(t, env, dir, "notify", ref, "--to", "alex", "--message", "Replace", "--follow-up", value)
		_ = requireCLIError(t, err, ExitValidation)
		after := fetchMetaIssueViaHTTP(t, env, pid, ref)
		require.Equal(t, before.Issue, after.Issue)
	}
	_, err := runCLICapture(t, env, dir, "notify", ref, "--to", "alex", "--clear", "--follow-up", "2099-11-11")
	_ = requireCLIError(t, err, ExitValidation)
	require.Contains(t, runCLI(t, env, dir, "inbox", "--for", "alex"), "Original")
}

func TestNotifyFollowUpWithReplyBroadcastAndQuiet(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Await review")
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	comment, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Teammate: "review", Body: "Finding"})
	require.NoError(t, err)
	out := runCLIAs(t, env, dir, "coordinator", "--json", "notify", ref, "--to", "reader/review", "--re", comment.UID,
		"--message", "Inspect", "--follow-up", "2099-11-10T09:30")
	require.Contains(t, out, `"scheduled_on":"2099-11-10T09:30"`)
	storedReply := fetchMetaIssueViaHTTP(t, env, pid, ref)
	var reply notificationValue
	require.NoError(t, json.Unmarshal([]byte(notificationMetadata(t, storedReply.Issue.Metadata)[notificationMetadataKey("reader/review")]), &reply))
	require.Equal(t, comment.UID, reply.Re)
	out = runCLIAs(t, env, dir, "coordinator", "--agent", "--quiet", "notify", ref, "--broadcast", "--teammates",
		"--message", "Inspect together", "--follow-up", "2099-11-11")
	require.Empty(t, strings.TrimSpace(out))
	stored := fetchMetaIssueViaHTTP(t, env, pid, ref)
	metadata := notificationMetadata(t, stored.Issue.Metadata)
	require.Equal(t, `"2099-11-11"`, metadata["scheduled_on"])
	for _, recipient := range []string{"reader", "reader/review"} {
		var request notificationValue
		require.NoError(t, json.Unmarshal([]byte(metadata[notificationMetadataKey(recipient)]), &request))
		require.Equal(t, notificationValue{From: "coordinator", Message: "Inspect together", Broadcast: true}, request)
	}
}

// Reaching the shared date reminds the current owner, or the author when unowned.
func TestNotifyFollowUpDueReminder(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprint(owned), func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Await external response")
			issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
			require.NoError(t, err)
			recipient := issue.Author
			if owned {
				_, _, _, err = env.DB.UpdateOwner(t.Context(), issue.ID, new("waiting-owner"), "coordinator")
				require.NoError(t, err)
				recipient = "waiting-owner"
			}
			runCLIAs(t, env, dir, "sender", "notify", ref, "--to", "alex-outside", "--message", "Please respond", "--follow-up", "2099-11-10T09:30")
			sweeper := daemon.NewDueNotificationSweeper(env.DB, daemon.NewEventPublisher(env.Broadcaster, nil), "UTC")
			require.NoError(t, sweeper.RunOnce(t.Context(), time.Date(2099, 11, 10, 9, 29, 0, 0, time.UTC)))
			require.Empty(t, strings.TrimSpace(runCLI(t, env, dir, "inbox", "--for", recipient, "--context")))
			require.NoError(t, sweeper.RunOnce(t.Context(), time.Date(2099, 11, 10, 9, 30, 0, 0, time.UTC)))
			require.Contains(t, runCLI(t, env, dir, "inbox", "--for", recipient), "Scheduled date reached: 2099-11-10T09:30")
			require.Contains(t, runCLI(t, env, dir, "inbox", "--for", "alex-outside"), "Please respond")
		})
	}
}

func TestNotifyFollowUpKeepsRecurrence(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Weekly review")
	runCLI(t, env, dir, "schedule", ref, "2099-11-02", "--repeat", "FREQ=WEEKLY", "--timezone", "UTC")
	before, err := env.DB.ListRecurrencesByProject(t.Context(), pid)
	require.NoError(t, err)
	require.Len(t, before, 1)
	original, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	runCLI(t, env, dir, "notify", ref, "--to", "alex", "--message", "Please decide", "--follow-up", "2099-11-04")
	after, err := env.DB.ListRecurrencesByProject(t.Context(), pid)
	require.NoError(t, err)
	require.Equal(t, before, after)
	updated, err := env.DB.IssueByID(t.Context(), original.ID)
	require.NoError(t, err)
	require.Equal(t, original.RecurrenceID, updated.RecurrenceID)
	require.Equal(t, original.OccurrenceKey, updated.OccurrenceKey)
	runCLI(t, env, dir, "close", ref, "--done", "--message", "Completed the weekly review and recorded the decision.", "--evidence", "test:review outcome checked")
	issues, err := env.DB.ListIssues(t.Context(), db.ListIssuesParams{ProjectID: pid})
	require.NoError(t, err)
	require.Len(t, issues, 2)
	for _, issue := range issues {
		if issue.ID != updated.ID {
			require.Equal(t, `"2099-11-09"`, notificationMetadata(t, []byte(issue.Metadata))["scheduled_on"])
		}
	}
}

// Baseline notify bodies have no scheduled_on and reject unknown object members.
func TestNotifyFollowUpOldDaemonRejectsWithoutFallback(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Old daemon compatibility")
	before, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	cursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	target, err := url.Parse(env.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var mutations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/notifications") || strings.HasSuffix(r.URL.Path, "/metadata")) {
			mutations.Add(1)
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/notifications") {
			// Fields of NotifyIssueRequest.Body before follow-up; validate the actual request.
			var oldBody struct {
				Actor     string `json:"actor"`
				Teammate  string `json:"teammate"`
				To        string `json:"to"`
				Re        string `json:"re"`
				Message   string `json:"message"`
				Broadcast bool   `json:"broadcast"`
				Teammates bool   `json:"teammates"`
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&oldBody); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": 400, "error": map[string]string{"code": "validation", "message": err.Error()}})
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	resetFlags(t)
	cmd := newRootCmd()
	cmd.SetContext(contextWithBaseURL(t.Context(), server.URL))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--workspace", dir, "notify", ref, "--to", "alex", "--message", "Ask", "--follow-up", "2099-11-10"})
	err = cmd.Execute()
	_ = requireCLIError(t, err, ExitValidation)
	require.Contains(t, err.Error(), "scheduled_on")
	require.Equal(t, int32(1), mutations.Load())
	after, err := env.DB.IssueByID(t.Context(), before.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	afterCursor, err := env.DB.MaxEventID(t.Context())
	require.NoError(t, err)
	require.Equal(t, cursor, afterCursor)
}
