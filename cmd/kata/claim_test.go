package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
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
	require.Contains(t, out, "assigned to agent1")
	require.NotContains(t, out, "no-op")

	// Verify the issue is assigned in the database
	iss := mustGetIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, iss.Owner)
	assert.Equal(t, "agent1", *iss.Owner)
}

func TestClaim_TTLAssignsUntilDaemonDeadline(t *testing.T) {
	env, dir := setupCLIEnv(t)
	pid := resolvePIDViaHTTP(t, env.URL, dir)
	issue := createIssueViaHTTPFull(t, env, dir, "timed claim")

	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID, "--ttl", "2m")

	assert.Contains(t, out, "assigned to agent1 until ")
	stored := mustGetTimedIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, stored.AssignmentExpiresOn)
	remaining := time.Until(*stored.AssignmentExpiresOn)
	assert.Greater(t, remaining, 115*time.Second)
	assert.LessOrEqual(t, remaining, 2*time.Minute)
}

func TestClaim_TTLValidation(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
	}{
		{value: "59s", want: "between 1m and 24h"},
		{value: "24h1s", want: "between 1m and 24h"},
		{value: "90.5s", want: "whole number of seconds"},
		{value: "120", want: "missing unit"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, _, err := executeRootCapture(t, context.Background(),
				"claim", "abcd", "--ttl", tc.value)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestClaim_AlreadyAssignedBySameActor(t *testing.T) {
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
	require.Contains(t, out, "already assigned to agent1")
	require.Contains(t, out, "no-op")

	// Verify the issue is still claimed in the database
	iss := mustGetIssueViaHTTP(t, env.URL, pid, issue.ShortID)
	require.NotNil(t, iss.Owner)
	assert.Equal(t, "agent1", *iss.Owner)
}

func TestClaim_IfUnownedRejectsSameActor(t *testing.T) {
	env, dir := setupCLIEnv(t)
	issue := createIssueViaHTTPFull(t, env, dir, "guarded claim")
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	_, err := runCLICapture(t, env, dir, "--as", "agent1", "claim", issue.ShortID, "--if-unowned")

	cliErr := requireCLIError(t, err, ExitConflict)
	assert.Contains(t, strings.ToLower(cliErr.Message), "already assigned")
	assert.NotContains(t, cliErr.Message, "--force")
}

func TestClaim_ForceAndIfUnownedAreMutuallyExclusive(t *testing.T) {
	_, _, err := executeRootCapture(t, context.Background(),
		"claim", "abcd", "--force", "--if-unowned")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "if-unowned")
	assert.Contains(t, err.Error(), "force")
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
	assert.Contains(t, strings.ToLower(ce.Message), "already assigned")
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
	require.Contains(t, out, "assigned to agent2")

	// Verify the issue is now assigned to agent2
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

	require.Contains(t, out, "assigned to agent2")
	require.Contains(t, out, "was: agent1")
}

func TestClaim_AgentOutputIncludesOwner(t *testing.T) {
	env, dir := setupCLIEnv(t)
	issue := createIssueViaHTTPFull(t, env, dir, "test claim agent")

	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent1", "--agent", "claim", issue.ShortID)

	assert.Regexp(t, `(?m)^OK claim \S+`, out)
	assert.Contains(t, out, "Owner: agent1")
}

func TestClaim_AgentForceOutputIncludesPreviousOwner(t *testing.T) {
	env, dir := setupCLIEnv(t)
	issue := createIssueViaHTTPFull(t, env, dir, "test claim agent previous")
	runCLIAs(t, env, dir, "agent1", "claim", issue.ShortID)

	resetFlags(t)
	out := runCLIAs(t, env, dir, "agent2", "--agent", "claim", "--force", issue.ShortID)

	assert.Regexp(t, `(?m)^OK claim \S+`, out)
	assert.Contains(t, out, "Owner: agent2")
	assert.Contains(t, out, "Previous-Owner: agent1")
}

func TestPrintClaimMutationIncludesAssignmentExpiry(t *testing.T) {
	const response = `{"issue":{"short_id":"abcd","owner":"agent1","assignment_expires_on":"2026-09-17T21:30:00Z"},"changed":true}`

	t.Run("human", func(t *testing.T) {
		resetFlags(t)
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)

		require.NoError(t, printClaimMutation(cmd, []byte(response)))
		assert.Equal(t, "abcd assigned to agent1 until 2026-09-17T21:30:00Z\n", out.String())
	})

	t.Run("agent", func(t *testing.T) {
		resetFlags(t)
		flags.Mode = outputAgent
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)

		require.NoError(t, printClaimMutation(cmd, []byte(response)))
		assert.Contains(t, out.String(), "Owner: agent1\n")
		assert.Contains(t, out.String(), "Assignment-Expires-On: 2026-09-17T21:30:00Z\n")
	})
}

func TestPrintClaimMutationTimedNoOpIncludesAssignmentExpiry(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	const response = `{"issue":{"short_id":"abcd","owner":"agent1","assignment_expires_on":"2026-09-17T21:30:00Z"},"changed":false}`

	require.NoError(t, printClaimMutation(cmd, []byte(response)))
	assert.Equal(t, "abcd already assigned to agent1 until 2026-09-17T21:30:00Z (no-op)\n", out.String())
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
	require.Contains(t, out, "assigned to agent1")
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

func mustGetTimedIssueViaHTTP(t *testing.T, baseURL string, pid int64, ref string) struct {
	Owner               *string    `json:"owner"`
	AssignmentExpiresOn *time.Time `json:"assignment_expires_on"`
} {
	t.Helper()
	type response struct {
		Issue struct {
			Owner               *string    `json:"owner"`
			AssignmentExpiresOn *time.Time `json:"assignment_expires_on"`
		} `json:"issue"`
	}
	resp := getJSON[response](t, baseURL+"/api/v1/projects/"+itoa(pid)+"/issues/"+ref)
	return resp.Issue
}
