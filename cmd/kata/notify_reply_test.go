package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestNotifyReAndBroadcastCLI(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	issue, _, e := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Finding", Author: "worker", Owner: new("owner")})
	require.NoError(t, e)
	c, _, e := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reader", Body: "Finding"})
	require.NoError(t, e)
	out, e := runCLICapture(t, env, dir, "notify", issue.ShortID, "--to", "reader", "--re", c.UID, "--message", "inspect")
	require.NoError(t, e)
	require.Contains(t, out, "notified")
	out, e = runCLICapture(t, env, dir, "--json", "notify", issue.ShortID, "--broadcast", "--re", c.UID, "--message", "all inspect")
	require.NoError(t, e)
	var result struct {
		Recipients []string `json:"recipients"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Contains(t, result.Recipients, "reader")
	require.Contains(t, result.Recipients, "owner")
}

func TestNotifyAdvancedFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"--broadcast", "--to", "reader", "--message", "inspect"},
		{"--teammates", "--to", "reader", "--message", "inspect"},
		{"--clear", "--to", "reader", "--re", "c:abc123"},
		{"--clear", "--broadcast"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			resetFlags(t)
			_, _, err := executeRootCapture(t, t.Context(), append([]string{"notify", "abcd"}, args...)...)
			_ = requireCLIError(t, err, ExitValidation)
		})
	}
}

// Canonical --reply must traverse the real CLI/API transaction hook and clear
// only the responding teammate's request for this exact comment.
func TestNotifyReplyClearsMatchingRequest(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Finding", Author: "coordinator"})
	require.NoError(t, err)
	target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "coordinator", Body: "Finding"})
	require.NoError(t, err)
	runCLIAs(t, env, dir, "coordinator", "notify", issue.ShortID, "--to", "reader/review", "--re", target.UID, "--message", "Inspect this finding")
	runCLIAs(t, env, dir, "coordinator", "notify", issue.ShortID, "--to", "reader", "--message", "Keep this human request")
	runCLIAs(t, env, dir, "reader", "--teammate", "review", "comment", issue.ShortID, "--reply", target.UID, "--body", "I have checked the requested finding.")
	current, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	require.NotContains(t, string(current.Metadata), notificationMetadataKey("reader/review"))
	require.Contains(t, string(current.Metadata), "Keep this human request")
}

// Any typed answer to the requested comment settles the request and tells
// the requester, on the issue where they asked, even when the comment has
// another author and the answer is posted on a different issue.
func TestNotifyTypedAnswerSettlesRequestAndNotifiesRequester(t *testing.T) {
	for _, tc := range []struct{ kind, flag string }{
		{"reply", "--reply"}, {"confirm", "--confirm"}, {"refute", "--refute"},
	} {
		// The request sits on the issue the coordinator waits on. The
		// requested comment, and the answer, can live on another issue.
		for _, layout := range []string{"same issue", "answer elsewhere", "comment and answer elsewhere"} {
			t.Run(tc.kind+"/"+layout, func(t *testing.T) {
				env, dir, pid := setupCLIWorkspace(t)
				issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Finding", Author: "coordinator"})
				require.NoError(t, err)
				other, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Follow-up", Author: "worker"})
				require.NoError(t, err)
				commentIssue, answerIssue := issue, issue
				if layout != "same issue" {
					answerIssue = other
				}
				if layout == "comment and answer elsewhere" {
					commentIssue = other
				}
				target, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: commentIssue.ID, Author: "reviewer", Body: "Finding"})
				require.NoError(t, err)
				runCLIAs(t, env, dir, "coordinator", "notify", issue.ShortID, "--to", "worker", "--re", target.UID, "--message", "Please confirm or refute this finding")
				runCLIAs(t, env, dir, "worker", "comment", answerIssue.ShortID, tc.flag, target.UID, "--body", "I reproduced the finding with the exact steps listed in the report above.")

				current, err := env.DB.IssueByID(t.Context(), issue.ID)
				require.NoError(t, err)
				var slots map[string]notificationValue
				require.NoError(t, json.Unmarshal([]byte(current.Metadata), &slots))
				require.NotContains(t, slots, notificationMetadataKey("worker"), "the answer settles the request")
				answered, ok := slots[notificationMetadataKey("coordinator")]
				require.True(t, ok, "the requester learns the request was answered: %s", current.Metadata)
				require.Equal(t, "worker", answered.From)
				require.Equal(t, tc.kind, answered.Kind)
				require.NotEmpty(t, answered.Re)
			})
		}
	}
}
