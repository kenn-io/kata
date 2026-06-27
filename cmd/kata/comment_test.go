package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComment_AppendsToIssue(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "x")

	out := runCLI(t, env, dir, "comment", short, "--body", "looks good")
	assert.True(t, strings.Contains(out, "looks good") || strings.Contains(out, "comment"))
}

func TestComment_MessageShorthandAppendsToIssue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "x")

	runCLI(t, env, dir, "comment", short, "-m", "looks good")

	issue := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, issue.Comments, 1)
	assert.Equal(t, "looks good", issue.Comments[0].Body)
}

func TestComment_EditUpdatesExistingCommentByUID(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "x")
	runCLI(t, env, dir, "comment", short, "--body", "token=leaked")
	issue := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, issue.Comments, 1)
	commentUID := issue.Comments[0].UID
	require.NotEmpty(t, commentUID)

	out := runCLI(t, env, dir, "comment", "edit", short, commentUID, "--body", "[redacted]")
	assert.True(t, strings.Contains(out, "comment edited") || strings.Contains(out, "comment"))

	updated := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, updated.Comments, 1)
	assert.Equal(t, commentUID, updated.Comments[0].UID)
	assert.Equal(t, "[redacted]", updated.Comments[0].Body)
	assert.NotContains(t, runCLI(t, env, dir, "show", short), "token=leaked")
	assert.Contains(t, runCLI(t, env, dir, "show", short), commentUID)
	assert.Contains(t, runCLI(t, env, dir, "--agent", "show", short), "uid="+commentUID)
}

func TestComment_RelationshipFlagSuggestsEditCommentComposition(t *testing.T) {
	resetRunEEntered(t)
	resetFlags(t)

	_, stderr, err := executeRootCapture(t, context.Background(),
		"comment", "abc4", "--blocks", "d4ex")

	require.Error(t, err)
	assert.Contains(t, stderr, "kata comment does not support --blocks")
	assert.Contains(t, stderr, `kata edit abc4 --blocks d4ex --comment "..."`)
}

func TestComment_AgentOutput(t *testing.T) {
	env, dir := setupCLIEnv(t)
	short := createIssueViaHTTP(t, env, dir, "x")

	out := runCLI(t, env, dir, "--agent", "comment", short, "--body", "looks good")

	assert.Regexp(t, `(?m)^OK comment \S+`, out)
	assert.Contains(t, out, "Comment: appended")
}
