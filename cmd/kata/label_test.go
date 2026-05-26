package main

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLabelAdd_HappyPath(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	out := runCLI(t, env, dir, "label", "add", ref, "needs-review")
	assert.Contains(t, out, "needs-review")
}

func TestLabelAdd_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	out := runCLI(t, env, dir, "--agent", "label", "add", ref, "needs-review")

	assert.Regexp(t, `(?m)^OK label \S+ changed=true`, out)
	assert.Contains(t, out, "Label: needs-review")
	assert.Contains(t, out, "Action: added")
}

func TestLabelRm_HappyPath(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	out := runCLI(t, env, dir, "label", "rm", ref, "bug")
	assert.True(t, strings.Contains(out, "removed") || strings.Contains(out, "unlabeled"))
}

func TestLabelRm_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "bug")

	resetFlags(t)
	out := runCLI(t, env, dir, "--agent", "label", "rm", ref, "bug")

	assert.Regexp(t, `(?m)^OK label \S+ changed=true`, out)
	assert.Contains(t, out, "Label: bug")
	assert.Contains(t, out, "Action: removed")
}

func TestLabelsList_PrintsCounts(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	out := runCLI(t, env, dir, "labels")
	assert.Contains(t, out, "bug")
	assert.Contains(t, out, "1")
}

func TestLabelsList_AgentOutputIncludesCount(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "bug")
	runCLI(t, env, dir, "label", "add", ref, "safari")
	out := runCLI(t, env, dir, "--agent", "labels")

	assert.Contains(t, out, "OK labels count=2\n")
	assert.Contains(t, out, "- label=bug count=1\n")
	assert.Contains(t, out, "- label=safari count=1")
}

// TestLabel_RejectsEmptyLabel covers hammer-test finding #8: label rm with an
// empty value used to URL-encode to "" and hit /labels/?actor=... which the
// daemon answered with a raw 404 page. Now both add and rm reject
// client-side with a uniform validation message.
func TestLabel_RejectsEmptyLabel(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "a")
	for _, args := range [][]string{
		{"label", "add", ref, ""},
		{"label", "rm", ref, "  "},
	} {
		resetFlags(t)
		_, err := runCLICapture(t, env, dir, args...)
		require.Errorf(t, err, "args %v should reject", args)
		ce := requireCLIError(t, err, ExitValidation)
		assert.Contains(t, ce.Message, "label must not be empty")
	}
}

func TestLabelAdd_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "a")

	runCLI(t, env, dir, "label", "add", ref, "flaky", "--comment", "intermittent in CI")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "intermittent in CI", got.Comments[0].Body)
}

func TestLabelRm_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "a")
	runCLI(t, env, dir, "label", "add", ref, "flaky")

	runCLI(t, env, dir, "label", "rm", ref, "flaky", "--comment", "stable since 4.6")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "stable since 4.6", got.Comments[0].Body)
}

// TestCreate_RejectsWhitespaceLabel covers the create --label case from
// hammer #8. Pflag's StringSliceVar drops a literal empty argument (""),
// but a whitespace-only label like "   " makes it through and used to be
// silently dropped by the daemon. Reject client-side instead.
func TestCreate_RejectsWhitespaceLabel(t *testing.T) {
	resetFlags(t)
	_, _, err := executeRootCapture(t, context.Background(), "create", "title", "--label", "   ")
	ce := requireCLIError(t, err, ExitValidation)
	assert.Contains(t, ce.Message, "label must not be empty")
}
