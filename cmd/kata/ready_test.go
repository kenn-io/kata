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

func TestReady_AllFlagListsAcrossProjects(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "in-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	// Text rows must use the qualified short-ref form: "<project>#<short_id>".
	// We don't pin the project name (depends on setupCLIWorkspace), but the
	// "#" separator is the contract.
	assert.Contains(t, out, "#",
		"--all output uses qualified refs (project#short_id), got: %q", out)
}

func TestReady_AllFlagJSONIncludesProjectName(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "first")

	out, err := runCmdOutput(t, env, "--workspace", dir, "--json", "ready", "--all")
	require.NoError(t, err)
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.NotEmpty(t, got.Issues)
	first := got.Issues[0]
	_, hasProject := first["project_name"]
	assert.True(t, hasProject, "project_name missing from --all JSON row: %v", first)
}

func TestReady_AllAndProjectAreMutuallyExclusive(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	_, err := runCmdOutput(t, env, "--workspace", dir,
		"--project", "anything", "ready", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestReady_AllFromBoundDirSkipsLocalProject pins that --all does not require
// (or use) the local .kata.toml project context: an agent in a bound workspace
// can still get the global view.
func TestReady_AllFromBoundDirSkipsLocalProject(t *testing.T) {
	env, dir, pid := setupCLIWorkspace(t)
	createIssue(t, env, pid, "from-bound-project")

	out, err := runCmdOutput(t, env, "--workspace", dir, "ready", "--all")
	require.NoError(t, err)
	assert.Contains(t, out, "#",
		"--all from bound dir still emits qualified refs, got: %q", out)
}

// TestReady_AllRejectsFilterFlags pins that --all errors out when combined
// with per-project filter flags rather than silently dropping them: the
// global ready endpoint does not apply --unowned / --owner / --label /
// --no-label, so accepting those alongside --all would return misleading
// (unfiltered) results.
func TestReady_AllRejectsFilterFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"unowned", []string{"ready", "--all", "--unowned"}},
		{"owner", []string{"ready", "--all", "--owner", "alice"}},
		{"label", []string{"ready", "--all", "--label", "bug"}},
		{"no-label", []string{"ready", "--all", "--no-label", "wip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, dir, _ := setupCLIWorkspace(t)
			args := append([]string{"--workspace", dir}, tc.args...)
			_, err := runCmdOutput(t, env, args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--all does not support")
		})
	}
}
