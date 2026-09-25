package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestIssueRefCommandsAcceptUIDsAndShortIDs(t *testing.T) { //nolint:paralleltest // testenv.New sets KATA_HOME and KATA_DB; newRootCmd resets package var flags
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)

	issue := func(title string) db.Issue {
		t.Helper()
		created := createIssueViaHTTPFull(t, env, dir, title)
		iss, err := env.DB.IssueByShortID(context.Background(), pid, created.ShortID, db.IncludeDeletedNo)
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
