package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaim_Success(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)

	// Create an unowned issue
	issue := createIssueViaHTTPFull(t, env, dir, "test claim")

	// Claim it as agent1
	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	// Should confirm claim
	require.Contains(t, out, issue.ShortID)
	require.Contains(t, out, "claimed by agent1")
	require.NotContains(t, out, "no-op")

	// Verify the issue is claimed in the database
	iss := mustGetIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, iss.Owner)
	assert.Equal(t, "agent1", *iss.Owner)
}

func TestClaim_AlreadyClaimedBySameActor(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)

	// Create and claim an issue as agent1
	issue := createIssueViaHTTPFull(t, env, dir, "test claim")
	resetFlags(t)
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	// Claim it again as agent1
	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	// Should show no-op
	require.Contains(t, out, issue.ShortID)
	require.Contains(t, out, "already claimed by agent1")
	require.Contains(t, out, "no-op")

	// Verify the issue is still claimed in the database
	iss := mustGetIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, iss.Owner)
	assert.Equal(t, "agent1", *iss.Owner)
}

func TestClaim_Conflict(t *testing.T) {
	env, dir := setupCLIEnv(t)

	// Create and claim an issue as agent1
	issue := createIssueViaHTTPFull(t, env, dir, "test claim conflict")
	resetFlags(t)
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	// Try to claim it as agent2 without force
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "--as", "agent2", "claim", issue.ShortID)

	// Should return conflict error
	require.Error(t, err)
	ce := requireCLIError(t, err, ExitConflict)
	assert.Contains(t, strings.ToLower(ce.Message), "already claimed")
}

func TestClaim_ForceOverride(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)

	// Create and claim an issue as agent1
	issue := createIssueViaHTTPFull(t, env, dir, "test claim force")
	resetFlags(t)
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	// Claim it as agent2 with force
	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent2", "claim", "--force", issue.ShortID)

	// Should succeed
	require.Contains(t, out, issue.ShortID)
	require.Contains(t, out, "claimed by agent2")

	// Verify the issue is now claimed by agent2
	iss := mustGetIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, iss.Owner)
	assert.Equal(t, "agent2", *iss.Owner)
}

func TestClaim_ForceOverrideShowsPreviousOwner(t *testing.T) {
	env, dir := setupCLIEnv(t)

	issue := createIssueViaHTTPFull(t, env, dir, "test claim force previous")
	resetFlags(t)
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent2", "claim", "--force", issue.ShortID)

	require.Contains(t, out, "claimed by agent2")
	require.Contains(t, out, "was: agent1")
}

func TestClaim_WithComment(t *testing.T) {
	env, dir := setupCLIEnv(t)

	// Create an unowned issue
	issue := createIssueViaHTTPFull(t, env, dir, "test claim with comment")

	// Claim it with a comment
	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID, "--comment", "Working on this now")

	// Should succeed
	require.Contains(t, out, issue.ShortID)
	require.Contains(t, out, "claimed by agent1")
}

// mustGetIssueViaHTTP retrieves an issue from the API and fails the test if it doesn't exist.
func mustGetIssueViaHTTP(t *testing.T, baseURL string, pid int64, ref string) struct {
	Owner *string `json:"owner"`
} {
	t.Helper()
	type response struct {
		Issue struct {
			Owner *string `json:"owner"`
		} `json:"issue"`
	}
	resp := getJSON[response](t, baseURL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
	return resp.Issue
}
