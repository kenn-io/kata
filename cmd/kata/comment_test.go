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
