package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueRefCommandsAcceptUIDsAndShortIDs(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)

	issue := func(title string) db.Issue {
		t.Helper()
		created := createIssueViaHTTPFull(t, env, dir, title)
		iss, err := env.DB.IssueByShortID(t.Context(), pid, created.ShortID, db.IncludeDeletedNo)
		require.NoError(t, err)
		return iss
	}
	run := func(args ...string) string {
		t.Helper()
		resetFlags(t)
		return runCLI(t, env, dir, args...)
	}

	assign := issue("assign by uid")
	out := run("assign", assign.UID, "alice")
	require.Contains(t, out, "alice")

	out = run("unassign", assign.ShortID)
	require.Contains(t, out, "unassigned")

	comment := issue("comment by uid")
	out = run("comment", comment.UID, "--body", "uid comment")
	require.True(t, strings.Contains(out, "uid comment") || strings.Contains(out, "comment"))

	label := issue("label by uid")
	out = run("label", "add", label.UID, "uid-label")
	require.Contains(t, out, "uid-label")
	out = run("label", "rm", label.ShortID, "uid-label")
	require.True(t, strings.Contains(out, "removed") || strings.Contains(out, "unlabeled"))

	edit := issue("edit by uid")
	out = run("edit", edit.UID, "--title", "edited through uid")
	require.Contains(t, out, "edited through uid")

	closeReopen := issue("close by uid")
	out = run("close", closeReopen.UID,
		"--reason", "wontfix",
		"--message", "Decided not to pursue this; it doesn't match the current product direction.")
	require.Contains(t, out, "closed")
	out = run("reopen", closeReopen.ShortID)
	require.Contains(t, out, "open")

	deleteRestore := issue("delete by uid")
	out = run("delete", deleteRestore.UID, "--force", "--confirm", fmt.Sprintf("DELETE kata#%s", deleteRestore.ShortID))
	require.Contains(t, out, "deleted")
	out = run("restore", deleteRestore.ShortID)
	require.Contains(t, out, "restored")

	purge := issue("purge by uid")
	out = run("purge", purge.UID, "--force", "--confirm", fmt.Sprintf("PURGE kata#%s", purge.ShortID))
	require.Contains(t, out, "purged")
}

func TestNumericProjectNameCollision(t *testing.T) {
	t.Run("project ID and qualified name", func(t *testing.T) {
		env, dir, _, target, _ := setupMoveCLIProjects(t)
		numeric, err := env.DB.CreateProject(t.Context(), fmt.Sprint(target.ID))
		require.NoError(t, err)
		namedIssue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: numeric.ID, Title: "numeric name issue", Author: "tester"})
		require.NoError(t, err)
		issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: target.ID, Title: "target issue", Author: "tester"})
		require.NoError(t, err)
		selector := fmt.Sprint(target.ID)
		out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "--project", selector, "show", issue.UID)
		require.NoError(t, err)
		require.Contains(t, out, issue.Title)
		_, _, err = executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "--project", selector, "comment", issue.UID, "--body", "recovered comment")
		require.NoError(t, err)
		stored := fetchIssueViaHTTPWithComments(t, env, target.ID, issue.ShortID)
		require.Len(t, stored.Comments, 1)
		require.Equal(t, "recovered comment", stored.Comments[0].Body)
		out, _, err = executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "--project", selector, "show", numeric.Name+"#"+namedIssue.ShortID)
		require.NoError(t, err)
		require.Contains(t, out, namedIssue.Title)
		out, _, err = executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), "--workspace", dir, "--project", selector, "comment", numeric.Name+"#"+namedIssue.ShortID, "--body", "named project comment")
		require.NoError(t, err)
		require.Contains(t, out, "comment appended")
		stored = fetchIssueViaHTTPWithComments(t, env, numeric.ID, namedIssue.ShortID)
		require.Len(t, stored.Comments, 1)
		require.Equal(t, "named project comment", stored.Comments[0].Body)
	})
	for _, tc := range []struct {
		command string
		uid     bool
	}{{"edit", false}, {"edit", true}, {"move", false}, {"move", true}, {"show", false}, {"comment", false}} {
		t.Run(fmt.Sprintf("%s/uid=%t", tc.command, tc.uid), func(t *testing.T) {
			env, dir, _, target, _ := setupMoveCLIProjects(t)
			named, err := env.DB.CreateProject(t.Context(), fmt.Sprint(target.ID))
			require.NoError(t, err)
			issue, _, err := env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: named.ID, Title: "named project issue", Author: "tester"})
			require.NoError(t, err)
			args := []string{"--workspace", dir, "--project", named.Name}
			ref := issue.ShortID
			if tc.uid {
				ref = issue.UID
			}
			switch tc.command {
			case "edit":
				args = append(args, "edit", ref, "--title", "updated issue")
			case "move":
				args = append(args, "move", ref, target.Name, "--single")
			case "show":
				args = append(args, "show", ref)
			case "comment":
				args = append(args, "comment", ref, "--body", "example comment")
			}
			out, _, err := executeRootCapture(t, contextWithBaseURL(t.Context(), env.URL), args...)
			require.NoError(t, err)
			switch tc.command {
			case "edit":
				require.Contains(t, out, "updated issue")
			case "move":
				stored, err := env.DB.IssueByUID(t.Context(), issue.UID, db.IncludeDeletedNo)
				require.NoError(t, err)
				require.Equal(t, target.ID, stored.ProjectID)
			case "show":
				require.Contains(t, out, issue.Title)
			case "comment":
				stored := fetchIssueViaHTTPWithComments(t, env, named.ID, issue.ShortID)
				require.Len(t, stored.Comments, 1)
				require.Equal(t, "example comment", stored.Comments[0].Body)
			}
		})
	}
}
