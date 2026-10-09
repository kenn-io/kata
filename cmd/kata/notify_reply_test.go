package main

import (
	"bytes"
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

func TestBroadcastRateHintAllCLIFormats(t *testing.T) {
	err := apiErrFromBody(429, []byte(`{"error":{"code":"broadcast_rate_limited","message":"broadcast limited","hint":"use targeted --to reader --re c:abc123 instead","data":{"retry_after_seconds":600}}}`))
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			switch mode {
			case "human":
				emitHumanError(&out, err, true)
			case "agent":
				emitAgentError(&out, "notify", err)
			case "json":
				emitJSONError(&out, err, true)
			}
			require.Contains(t, out.String(), "--to reader --re c:abc123")
			if mode == "json" {
				require.Contains(t, out.String(), `"hint"`)
			}
		})
	}
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
