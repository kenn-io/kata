package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreate_WithMetadataPersists pins that `kata create --meta key=value`
// forwards metadata that round-trips on show --json.
func TestCreate_WithMetadataPersists(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)

	short := runCLI(t, env, dir, "--quiet", "create", "meta issue",
		"--meta", "work.branch=feature/x",
		"--meta", "work.attention=stuck")
	short = trimLine(short)

	out := runCLI(t, env, dir, "--json", "show", short)
	var resp struct {
		Issue struct {
			Metadata map[string]json.RawMessage `json:"metadata"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &resp))
	assert.JSONEq(t, `"feature/x"`, string(resp.Issue.Metadata["work.branch"]))
	assert.JSONEq(t, `"stuck"`, string(resp.Issue.Metadata["work.attention"]))
}

// TestCreate_MetaMissingEqualsIsUsageError pins that a --meta without "=" is a
// local usage error.
func TestCreate_MetaMissingEqualsIsUsageError(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_, err := runCLICapture(t, env, dir, "create", "bad meta", "--meta", "noequals")
	require.Error(t, err)
	_ = requireCLIError(t, err, ExitUsage)
}

// TestList_MetaFilterEquality pins `kata list --meta key=value`.
func TestList_MetaFilterEquality(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	stuck := trimLine(runCLI(t, env, dir, "--quiet", "create", "stuck one",
		"--meta", "work.attention=stuck"))
	_ = runCLI(t, env, dir, "--quiet", "create", "ok one", "--meta", "work.attention=ok")

	out := runCLI(t, env, dir, "list", "--meta", "work.attention=stuck")
	assert.Contains(t, out, stuck)
	assert.NotContains(t, out, "ok one")
	assert.Contains(t, out, "stuck one")
}

// TestList_MetaFilterPresence pins `kata list --meta key` (presence).
func TestList_MetaFilterPresence(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_ = runCLI(t, env, dir, "--quiet", "create", "branched", "--meta", "work.branch=feature/x")
	_ = runCLI(t, env, dir, "--quiet", "create", "unbranched")

	out := runCLI(t, env, dir, "list", "--meta", "work.branch")
	assert.Contains(t, out, "branched")
	assert.NotContains(t, out, "unbranched")
}

func TestList_RepeatedMetaFiltersRequireEveryMatch(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	_ = runCLI(t, env, dir, "--quiet", "create", "matching metadata",
		"--meta", "team=core", "--meta", "tier=one")
	_ = runCLI(t, env, dir, "--quiet", "create", "partial metadata",
		"--meta", "team=core")

	out := runCLI(t, env, dir, "list", "--meta", "team=core", "--meta", "tier=one")
	assert.Contains(t, out, "matching metadata")
	assert.NotContains(t, out, "partial metadata")
}

// TestShow_HumanRendersMetadataSection pins the human-mode metadata section.
func TestShow_HumanRendersMetadataSection(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	short := trimLine(runCLI(t, env, dir, "--quiet", "create", "with meta",
		"--meta", "work.branch=feature/x"))

	out := runCLI(t, env, dir, "show", short)
	assert.Contains(t, out, "--- metadata ---")
	assert.Contains(t, out, `work.branch = "feature/x"`)
}

// TestShow_HumanOmitsMetadataSectionWhenEmpty pins that no section renders when
// the issue has no metadata.
func TestShow_HumanOmitsMetadataSectionWhenEmpty(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	short := trimLine(runCLI(t, env, dir, "--quiet", "create", "plain"))

	out := runCLI(t, env, dir, "show", short)
	assert.NotContains(t, out, "--- metadata ---")
}

// TestShow_AgentRendersMetadataRows pins agent-mode metadata rows.
func TestShow_AgentRendersMetadataRows(t *testing.T) {
	env, dir, _ := setupCLIWorkspace(t)
	short := trimLine(runCLI(t, env, dir, "--quiet", "create", "agent meta",
		"--meta", "work.attention=stuck"))

	out := runCLI(t, env, dir, "--agent", "show", short)
	assert.Contains(t, out, "Metadata:")
	// The value is compact JSON (`"stuck"`) passed through agentValue, which
	// escapes the embedded quotes like every other agent row.
	assert.Contains(t, out, `- key=work.attention value="\"stuck\""`)
}

// trimLine strips a single trailing newline from quiet-mode output.
func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
