package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReady_OutputsShortIDNotNumber pins the JSON wire shape: each ready
// row carries short_id; the legacy `number` field is gone.
func TestReady_OutputsShortIDNotNumber(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "first")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "ready")
	require.NoError(t, err)
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasShort := first["short_id"]
	_, hasNumber := first["number"]
	assert.True(t, hasShort, "short_id missing from ready row: %v", first)
	assert.False(t, hasNumber, "number still present in ready row: %v", first)
}

func TestReady_FiltersBlocked(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	blocker := createIssue(t, env, pid, "blocker")
	blocked := createIssue(t, env, pid, "blocked")
	createIssue(t, env, pid, "standalone")
	createLinkViaHTTP(t, env, pid, blocker, "blocks", blocked)

	out := runCLI(t, env, dir, "ready")
	assert.Contains(t, out, "blocker")
	assert.Contains(t, out, "standalone")
	assert.False(t, strings.Contains(out, "blocked"),
		"blocked is hidden while blocker is open")
}

func TestReady_AgentOutputRowsOmitAbsentOwner(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "ready unowned")

	out := runCLI(t, env, dir, "--agent", "ready")

	assert.Contains(t, out, "OK ready count=1\n")
	assert.Contains(t, out, `title="ready unowned"`)
	assert.NotContains(t, out, "owner=")
}

func TestReady_UnownedAndOwnerMutualExclusion(t *testing.T) {
	env, dir := setupCLIEnv(t)
	resetFlags(t)
	_, err := runCLICapture(t, env, dir, "ready", "--unowned", "--owner", "alice")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}
