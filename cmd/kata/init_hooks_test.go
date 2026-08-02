package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	var settings map[string]any
	require.NoError(t, json.Unmarshal(data, &settings))
	return settings
}

func expectedHookHandler(mode string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata attention-hook " + mode,
	}
}

func TestApplyClaudeHooks_RefusesSymlinks(t *testing.T) {
	t.Run("config directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, ".claude")))

		_, err := applyClaudeHooks(dir)
		require.Error(t, err)

		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})

	t.Run("settings file", func(t *testing.T) {
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
		assert.Equal(t, "{}", string(got))
	})
}
