package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests for `kata init --with-hooks`: installing the work.attention
// harness hooks (script + .claude/settings.json wiring) for Claude Code.
// They complement the e2e coverage in init_with_hooks_e2e_test.go.

// readSettings decodes .claude/settings.json under dir for structural asserts.
func readSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(bs, &m))
	return m
}

// hookCommands flattens every command string wired under one hook event.
func hookCommands(t *testing.T, settings map[string]any, event string) []string {
	t.Helper()
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var cmds []string
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if c, ok := em["command"].(string); ok {
				cmds = append(cmds, c)
			}
		}
	}
	return cmds
}

func TestApplyClaudeHooks_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)

	// The hook script exists, is executable, and carries the three modes.
	scriptPath := filepath.Join(dir, ".claude", "hooks", "kata-attention.sh")
	fi, err := os.Stat(scriptPath)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&0o111, "hook script must be executable")
	script, err := os.ReadFile(scriptPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	for _, mode := range []string{"start)", "claim)", "stop)"} {
		assert.Contains(t, string(script), mode)
	}

	// settings.json wires the script into all three events.
	settings := readSettings(t, dir)
	assert.Contains(t, hookCommands(t, settings, "SessionStart"), claudeHookCommand("start"))
	assert.Contains(t, hookCommands(t, settings, "PostToolUse"), claudeHookCommand("claim"))
	assert.Contains(t, hookCommands(t, settings, "Stop"), claudeHookCommand("stop"))

	// The PostToolUse wiring is scoped to Bash tool calls.
	hooks := settings["hooks"].(map[string]any)
	groups := hooks["PostToolUse"].([]any)
	require.Len(t, groups, 1)
	assert.Equal(t, "Bash", groups[0].(map[string]any)["matcher"])
}

func TestApplyClaudeHooks_PreservesExistingSettings(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	existing := `{
  "permissions": {"allow": ["WebFetch"]},
  "hooks": {
    "PostToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "lint.sh"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "notify.sh"}]}
    ]
  }
}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(existing), 0o644)) //nolint:gosec // test fixture under TempDir

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)

	settings := readSettings(t, dir)
	// Pre-existing config survives untouched.
	perms := settings["permissions"].(map[string]any)
	assert.Equal(t, []any{"WebFetch"}, perms["allow"])
	assert.Contains(t, hookCommands(t, settings, "PostToolUse"), "lint.sh")
	assert.Contains(t, hookCommands(t, settings, "Stop"), "notify.sh")
	// kata's wiring is appended alongside.
	assert.Contains(t, hookCommands(t, settings, "PostToolUse"), claudeHookCommand("claim"))
	assert.Contains(t, hookCommands(t, settings, "Stop"), claudeHookCommand("stop"))
	assert.Contains(t, hookCommands(t, settings, "SessionStart"), claudeHookCommand("start"))
}

func TestApplyClaudeHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	require.True(t, changed)
	settingsBefore, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	scriptBefore, err := os.ReadFile(filepath.Join(dir, ".claude", "hooks", "kata-attention.sh")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	changed, err = applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.False(t, changed, "second run must be a no-op")
	settingsAfter, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	scriptAfter, err := os.ReadFile(filepath.Join(dir, ".claude", "hooks", "kata-attention.sh")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, string(settingsBefore), string(settingsAfter))
	assert.Equal(t, string(scriptBefore), string(scriptAfter))
}

func TestApplyClaudeHooks_RewritesDriftedScript(t *testing.T) {
	dir := t.TempDir()
	_, err := applyClaudeHooks(dir)
	require.NoError(t, err)

	scriptPath := filepath.Join(dir, ".claude", "hooks", "kata-attention.sh")
	require.NoError(t, os.WriteFile(scriptPath, []byte("#!/bin/sh\n# stale\n"), 0o755)) //nolint:gosec // test fixture under TempDir

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed, "drifted managed script must be refreshed")
	script, err := os.ReadFile(scriptPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(script), "kata meta set")
}

func TestApplyClaudeHooks_MalformedSettingsLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	malformed := "{ not json"
	settingsPath := filepath.Join(claudeDir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(malformed), 0o644)) //nolint:gosec // test fixture under TempDir

	_, err := applyClaudeHooks(dir)
	require.Error(t, err)

	got, err := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, malformed, string(got), "malformed settings must not be clobbered")
}

func TestApplyClaudeHooks_RefusesSymlinkedSettings(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	victim := filepath.Join(dir, "victim.json")
	require.NoError(t, os.WriteFile(victim, []byte("{}"), 0o644)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.Symlink(victim, filepath.Join(claudeDir, "settings.json")))

	_, err := applyClaudeHooks(dir)
	require.Error(t, err)

	got, err := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, "{}", string(got), "symlink target must not be written through")
}

func TestApplyClaudeHooks_RefusesSymlinkedScript(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, ".claude", "hooks")
	require.NoError(t, os.MkdirAll(hooksDir, 0o750))
	victim := filepath.Join(dir, "victim.sh")
	require.NoError(t, os.WriteFile(victim, []byte("original"), 0o644)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.Symlink(victim, filepath.Join(hooksDir, "kata-attention.sh")))

	_, err := applyClaudeHooks(dir)
	require.Error(t, err)

	got, err := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, "original", string(got), "symlink target must not be written through")
}
