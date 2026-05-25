package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// extractShortID parses JSON output from a create command and extracts the short_id.
func extractShortID(t *testing.T, jsonOutput string) string {
	t.Helper()
	type createResp struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	var resp createResp
	require.NoError(t, json.Unmarshal([]byte(jsonOutput), &resp),
		"failed to parse JSON output: %s", jsonOutput)
	require.NotEmpty(t, resp.Issue.ShortID, "short_id missing from JSON output")
	return resp.Issue.ShortID
}

func TestAgentClaimWorkflow(t *testing.T) {
	// Setup
	env, dir := setupCLIEnv(t)

	// Create two issues, one owned, one unowned
	resetFlags(t)
	out := runCLI(t, env, dir, "create", "unowned issue", "--json")
	unownedID := extractShortID(t, out)

	resetFlags(t)
	out = runCLI(t, env, dir, "create", "owned issue", "--json")
	ownedID := extractShortID(t, out)
	resetFlags(t)
	runCLI(t, env, dir, "assign", ownedID, "alice")

	// kata ready --unowned returns only the unowned one
	resetFlags(t)
	out = runCLI(t, env, dir, "ready", "--unowned")
	assert.Contains(t, out, unownedID)
	assert.NotContains(t, out, ownedID)

	// kata claim succeeds
	resetFlags(t)
	out = runCLI(t, env, dir, "claim", unownedID, "--as", "agent1")
	assert.Contains(t, out, "claimed by agent1")

	// kata ready --unowned no longer returns it
	resetFlags(t)
	out = runCLI(t, env, dir, "ready", "--unowned")
	assert.NotContains(t, out, unownedID)

	// Second claim attempt by different actor returns conflict
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "claim", unownedID, "--as", "agent2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already claimed")
}
