package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestCloseVerificationCLI(t *testing.T) {
	for _, overrule := range []bool{false, true} {
		t.Run(map[bool]string{false: "warn", true: "overrule"}[overrule], func(t *testing.T) {
			env, dir, pid, ref := setupWorkspaceWithIssue(t, "Claim")
			issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
			require.NoError(t, err)
			claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
			require.NoError(t, err)
			r, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "This claim does not satisfy the completion contract.", ReplyToUID: claim.UID, ReplyKind: "refute"})
			require.NoError(t, err)
			args := []string{"close", ref, "--done", "-m", "Implemented the feature and checked the focused package.", "--test", "go test ./example"}
			if overrule {
				args = append(args, "--overrule", "c:"+strings.ToLower(r.UID[len(r.UID)-6:]))
			}
			stdout, stderr, err := runCLIWithErr(t, env, dir, args...)
			require.NoError(t, err)
			require.Contains(t, stdout, "closed")
			if overrule {
				require.NotContains(t, stderr, "warning:")
				audit := runCLI(t, env, dir, "--agent", "audit", "closes")
				require.Contains(t, audit, "overruled=")
			} else {
				require.Contains(t, stderr, "warning: open_refutations:")
			}
		})
	}
}

func TestCloseRefutationTerminalOutputSanitization(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssueOptions(t, "Claim", func(cfg *daemon.ServerConfig) {
		cfg.CloseVerification = "block"
	})
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "worker", Body: "Completion claim",
	})
	require.NoError(t, err)
	unsafeSummary := "hostile \x1b[2Jclear-screen \x1b]0;spoof-title\x07 tail"
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "reviewer", Body: unsafeSummary,
		ReplyToUID: claim.UID, ReplyKind: "refute",
	})
	require.NoError(t, err)
	args := []string{"close", ref, "--done", "-m", "Implemented the fix and checked the focused tests.", "--test", "go test ./example"}
	_, humanStderr, err := runCLIWithErr(t, env, dir, args...)
	require.Error(t, err)
	require.NotContains(t, humanStderr, "\x1b")
	require.NotContains(t, humanStderr, "\x07")
	require.Contains(t, humanStderr, "open refutations")

	_, jsonStderr, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL),
		append([]string{"--workspace", dir, "--json"}, args...)...)
	require.Error(t, err)
	var jsonError struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonStderr), &jsonError))
	require.Contains(t, jsonError.Error.Message, unsafeSummary,
		"JSON errors preserve the daemon's raw message")
}

func TestCloseWarningSanitizesTerminalButPreservesJSON(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Claim")
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "worker", Body: "Completion claim",
	})
	require.NoError(t, err)
	unsafeSummary := "hostile \x1b[2Jclear-screen \x1b]0;spoof-title\x07 tail"
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{
		IssueID: issue.ID, Author: "reviewer", Body: unsafeSummary,
		ReplyToUID: claim.UID, ReplyKind: "refute",
	})
	require.NoError(t, err)
	stdout, stderr, err := runCLIWithErr(t, env, dir,
		"--json", "close", ref, "--done", "-m", "Implemented the fix and checked the focused tests.",
		"--test", "go test ./example")
	require.NoError(t, err, stderr)
	require.NotContains(t, stderr, "\x1b")
	require.NotContains(t, stderr, "\x07")
	var response struct {
		Warnings []struct {
			Refutations []struct {
				Summary string `json:"summary"`
			} `json:"refutations"`
		} `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &response))
	require.Len(t, response.Warnings, 1)
	require.Equal(t, unsafeSummary, response.Warnings[0].Refutations[0].Summary,
		"JSON responses preserve the daemon's raw summary")
}

func TestShowVerificationRender(t *testing.T) {
	var b showResponseForCLI
	require.NoError(t, json.Unmarshal([]byte(`{"issue":{"uid":"issue","short_id":"abc4","title":"Claim","status":"open","author":"worker"},"verification":{"required":true,"confirmed":[{"comment":"c:abc123","confirmations":1}],"open_refutations":[{"refutation":"c:def456","target":"c:abc123","author":"reviewer"}],"redacted":2}}`), &b))
	var human, agent bytes.Buffer
	require.NoError(t, printShowHuman(&human, b, "example-project", nil))
	require.Contains(t, human.String(), "Verification: required; 1 confirmed claim (c:abc123); 1 open refutation (c:def456 → c:abc123); 2 redacted")
	require.NoError(t, printShowAgent(&agent, b, "example-project", "show"))
	require.Contains(t, agent.String(), "Verification: required;")
}

func TestShowWithoutVerificationGolden(t *testing.T) {
	var b showResponseForCLI
	b.Issue.ShortID = "abc4"
	b.Issue.Title = "Claim"
	b.Issue.Status = "open"
	b.Issue.Author = "worker"
	var out bytes.Buffer
	require.NoError(t, printShowHuman(&out, b, "example-project", nil))
	require.Equal(t, "abc4  Claim  [open]  by worker\n", out.String())
	out.Reset()
	require.NoError(t, printShowAgent(&out, b, "example-project", "show"))
	require.Equal(t, "OK show abc4\nIssue: abc4 Claim\nStatus: open\nRevision: 0\nBody:\n```text\n\n```\n", out.String())
}

func TestCloseVerdictEvidenceCLI(t *testing.T) {
	for _, kind := range []string{"confirmed", "refuted"} {
		for _, canonical := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "-sugar", true: "-canonical"}[canonical], func(t *testing.T) {
				env, dir, pid, ref := setupWorkspaceWithIssue(t, "Completion claim")
				issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
				require.NoError(t, err)
				claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Claim"})
				require.NoError(t, err)
				reply := "confirm"
				reason := "--done"
				if kind == "refuted" {
					reply = "refute"
					reason = "--wontfix"
				}
				_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Independent verdict", ReplyToUID: claim.UID, ReplyKind: reply})
				require.NoError(t, err)
				handle := "c:" + strings.ToLower(claim.UID[len(claim.UID)-6:])
				args := []string{"--agent", "close", ref, reason, "-m", strings.Repeat("Reviewed the claim against the requested behavior. ", 2)}
				if canonical {
					args = append(args, "--evidence", kind+":"+handle)
				} else {
					args = append(args, "--"+kind, handle)
				}
				stdout, stderr, err := runCLIWithErr(t, env, dir, args...)
				require.NoError(t, err, stderr)
				require.Contains(t, stdout, kind+" "+handle)
				audit := runCLI(t, env, dir, "audit", "closes")
				require.Contains(t, audit, kind+" "+handle)
				shown := runCLI(t, env, dir, "show", ref)
				require.Contains(t, shown, kind+" "+handle)
			})
		}
	}
}

// M2: receipts retain comment UIDs when an authorized view needs longer handles.
func TestCloseCitationSurvivesCollisionAndMove(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "Completion claim")
	issue, err := env.DB.IssueByShortID(t.Context(), pid, ref, db.IncludeDeletedNo)
	require.NoError(t, err)
	claim, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Completion claim"})
	require.NoError(t, err)
	_, _, err = env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: issue.ID, Author: "reviewer", Body: "Independent confirmation", ReplyToUID: claim.UID, ReplyKind: "confirm"})
	require.NoError(t, err)
	original := "c:" + strings.ToLower(claim.UID[len(claim.UID)-6:])
	args := []string{"--json", "close", ref, "--done", "--confirmed", original, "-m", "Completed the requested fix and checked the focused tests.", "--idempotency-key", "cited-close"}
	runCLI(t, env, dir, args...)
	other, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Author: "worker", Title: "Another claim"})
	require.NoError(t, err)
	collision, _, err := env.DB.CreateComment(t.Context(), db.CreateCommentParams{IssueID: other.ID, Author: "worker", Body: "Unrelated claim"})
	require.NoError(t, err)
	prefix := "01AAAAAAAAAAAAAAAAAA"
	if claim.UID[len(claim.UID)-7] == 'A' {
		prefix = prefix[:19] + "B"
	}
	// Fixture-only DML makes a legitimate ULID suffix collision deterministic.
	_, err = env.DB.ExecContext(t.Context(), "UPDATE comments SET uid = ? WHERE id = ?", prefix+claim.UID[len(claim.UID)-6:], collision.ID)
	require.NoError(t, err)
	current := "c:" + strings.ToLower(claim.UID[len(claim.UID)-7:])
	replay := runCLI(t, env, dir, args...)
	require.Contains(t, replay, `"reused":true`)
	require.Contains(t, replay, current)
	audit := runCLI(t, env, dir, "audit", "closes")
	require.Contains(t, audit, "confirmed "+current)
	destination, err := env.DB.CreateProject(t.Context(), "moved-project")
	require.NoError(t, err)
	live, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	_, err = env.DB.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: pid, ToProjectID: destination.ID, IfMatchRev: live.Revision, Actor: "worker"})
	require.NoError(t, err)
	moved, err := env.DB.IssueByID(t.Context(), issue.ID)
	require.NoError(t, err)
	shown := runCLI(t, env, dir, "show", destination.Name+"#"+moved.ShortID)
	require.Contains(t, shown, "confirmed "+original, "the historical citation uses its current authorized handle")
}
