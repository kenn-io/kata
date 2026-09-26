package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostFollowupCommentFailureRecommendsSafeKeyedRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "temporary failure", http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	err := postFollowupCommentWithKey(t.Context(), server.Client(), server.URL,
		1, "abc1", "example-agent", "finished work", "", "close-comment:close-request-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rerun the original kata close command with the same --idempotency-key")
}

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

func TestComment_MessageAliasAppendsToIssue(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "x")

	runCLI(t, env, dir, "comment", short, "--message", "looks good")

	issue := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, issue.Comments, 1)
	assert.Equal(t, "looks good", issue.Comments[0].Body)
}

func TestComment_MessageAliasIsTheBodyFlag(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "x")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	require.NoError(t, os.WriteFile(bodyFile, []byte("from file"), 0o600))

	runCLI(t, env, dir, "comment", short, "--body", "first", "--message", "second")
	runCLI(t, env, dir, "comment", short, "--message", "third", "--body", "fourth")
	issue := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, issue.Comments, 2)
	assert.Equal(t, "second", issue.Comments[0].Body)
	assert.Equal(t, "fourth", issue.Comments[1].Body)

	_, out, err := runCLIWithErr(t, env, dir, "comment", short, "--message", "")
	require.Error(t, err)
	assert.Contains(t, out, "comment body is required")

	for _, value := range []string{"x", ""} {
		_, out, err = runCLIWithErr(t, env, dir, "comment", short, "--message", value, "--body-file", bodyFile)
		require.Error(t, err)
		assert.Contains(t, out, "must pass exactly one of")
		_, out, err = runCLIWithErr(t, env, dir, "comment", short, "--message", value, "--body-stdin")
		require.Error(t, err)
		assert.Contains(t, out, "must pass exactly one of")
	}
}

func TestComment_EditAcceptsMessageAlias(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	short := createIssue(t, env, pid, "x")
	runCLI(t, env, dir, "comment", short, "--body", "draft")
	commentUID := fetchIssueViaHTTPWithComments(t, env, pid, short).Comments[0].UID

	runCLI(t, env, dir, "comment", "edit", short, commentUID, "--message", "final")

	updated := fetchIssueViaHTTPWithComments(t, env, pid, short)
	require.Len(t, updated.Comments, 1)
	assert.Equal(t, "final", updated.Comments[0].Body)

	_, out, err := runCLIWithErr(t, env, dir, "comment", "edit", short, commentUID, "--message", "")
	require.Error(t, err)
	assert.Contains(t, out, "comment body is required")
	_, out, err = runCLIWithErr(t, env, dir, "comment", "edit", short, commentUID, "--body", "a", "--message", "b", "--body-stdin")
	require.Error(t, err)
	assert.Contains(t, out, "must pass exactly one of")
}
