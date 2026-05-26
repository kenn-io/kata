package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssign_RoundTrip(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "x")

	out := runCLI(t, env, dir, "assign", ref, "alice")
	assert.True(t, strings.Contains(out, "assigned") ||
		strings.Contains(out, "alice"))

	uOut := runCLI(t, env, dir, "unassign", ref)
	assert.True(t, strings.Contains(uOut, "unassigned"))
}

func TestAssign_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "x")

	out := runCLI(t, env, dir, "--agent", "assign", ref, "wesm")

	assert.Regexp(t, `(?m)^OK assign \S+ changed=true`, out)
	assert.Contains(t, out, "Owner: wesm")
}

func TestUnassign_AgentOutput(t *testing.T) {
	env, dir, _, ref := setupWorkspaceWithIssue(t, "x")
	runCLI(t, env, dir, "assign", ref, "wesm")

	resetFlags(t)
	out := runCLI(t, env, dir, "--agent", "unassign", ref)

	assert.Regexp(t, `(?m)^OK unassign \S+ changed=true`, out)
	assert.Contains(t, out, "Owner-Cleared: true")
}

func TestAssign_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "x")

	runCLI(t, env, dir, "assign", ref, "alice", "--comment", "owns the auth area")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "owns the auth area", got.Comments[0].Body)
}

func TestUnassign_WithComment_AppendsComment(t *testing.T) {
	env, dir, pid, ref := setupWorkspaceWithIssue(t, "x")
	runCLI(t, env, dir, "assign", ref, "alice")

	runCLI(t, env, dir, "unassign", ref, "--comment", "rolling off")

	got := fetchIssueViaHTTPWithComments(t, env, pid, ref)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "rolling off", got.Comments[0].Body)
}
