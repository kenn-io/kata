package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readCodexHooks(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	var hooks map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&hooks))
	return hooks
}

func expectedCodexHandler() map[string]any {
	return map[string]any{
		"type":           "command",
		"command":        "kata attention-hook start --source kata-agent-hook-start",
		"commandWindows": "kata attention-hook start --source kata-agent-hook-start",
		"timeout":        json.Number("10"),
	}
}

func TestApplyCodexHooks_AdoptsPreviousCommand(t *testing.T) {
	dir := t.TempDir()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	legacy := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"kata attention-hook start","timeout":10},{"type":"command","command":"kata attention-hook start","commandWindows":"kata attention-hook start","timeout":10}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(legacy), 0o644)) //nolint:gosec // test fixture under TempDir

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, warnings)
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": codexSessionStartMatcher,
				"hooks":   []any{expectedCodexHandler()},
			}},
		},
	}, readCodexHooks(t, dir))
}

func TestApplyCodexHooks_PreservesCommandsContainingOldMarker(t *testing.T) {
	dir := t.TempDir()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	userCommand := "notify-wrapper kata attention-hook start"
	settings := `{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear","hooks":[{"type":"command","command":"` + userCommand + `"},{"type":"command","command":"kata attention-hook start","timeout":11}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "hooks.json"), []byte(settings), 0o644)) //nolint:gosec // test fixture under TempDir

	_, _, err := applyCodexHooks(dir)
	require.NoError(t, err)
	hooks := readCodexHooks(t, dir)["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	assert.Equal(t, []any{
		map[string]any{"type": "command", "command": userCommand},
		map[string]any{"type": "command", "command": "kata attention-hook start", "timeout": json.Number("11")},
	}, groups[0].(map[string]any)["hooks"])
}

func TestApplyCodexHooks_RefusesSymlinks(t *testing.T) {
	t.Run("config directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, ".codex")))

		_, _, err := applyCodexHooks(dir)
		require.Error(t, err)

		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("hooks file", func(t *testing.T) {
		dir := t.TempDir()
		codexDir := filepath.Join(dir, ".codex")
		require.NoError(t, os.MkdirAll(codexDir, 0o750))
		victim := filepath.Join(dir, "victim.json")
		require.NoError(t, os.WriteFile(victim, []byte("{}"), 0o644)) //nolint:gosec // test fixture under TempDir
		require.NoError(t, os.Symlink(victim, filepath.Join(codexDir, "hooks.json")))

		_, _, err := applyCodexHooks(dir)
		require.Error(t, err)

		got, err := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
		require.NoError(t, err)
		assert.Equal(t, "{}", string(got))
	})
}

func TestApplyCodexHooks_WarnsOnConfigTomlHooks(t *testing.T) {
	dir := t.TempDir()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte("[hooks]\nSessionStart = []\n"), 0o644)) //nolint:gosec // test fixture under TempDir

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "already defines a [hooks] table")
}
