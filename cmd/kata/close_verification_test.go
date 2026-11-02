package main

import (
	"bytes"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
