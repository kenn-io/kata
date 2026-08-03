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
		"command": "kata attention-hook " + mode + " --source kata-agent-hook-" + mode,
	}
}

func TestApplyClaudeHooks_MigratesExactLegacyHandlers(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	legacy := `{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear","hooks":[{"type":"command","command":"kata","args":["attention-hook","start"]},{"type":"command","command":"kata attention-hook start"},{"type":"command","command":"notify-session"}]}],"SessionEnd":[{"matcher":"logout|prompt_input_exit|bypass_permissions_disabled|other","hooks":[{"type":"command","command":"kata","args":["attention-hook","end"]},{"type":"command","command":"kata attention-hook end"}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(legacy), 0o644)) //nolint:gosec // test fixture under TempDir

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "startup|resume|clear",
					"hooks": []any{
						map[string]any{"type": "command", "command": "notify-session"},
					},
				},
				map[string]any{
					"matcher": "startup|resume|clear",
					"hooks":   []any{expectedHookHandler("start")},
				},
			},
			"SessionEnd": []any{map[string]any{
				"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
				"hooks":   []any{expectedHookHandler("end")},
			}},
		},
	}, readSettings(t, dir))
}

func TestApplyClaudeHooks_PreservesCommandsContainingOldMarker(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	userCommand := "notify-wrapper kata attention-hook start"
	settings := `{"hooks":{"SessionStart":[{"matcher":"startup|resume|clear","hooks":[{"type":"command","command":"` + userCommand + `"},{"type":"command","command":"kata attention-hook start","timeout":5}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(settings), 0o644)) //nolint:gosec // test fixture under TempDir

	_, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	hooks := readSettings(t, dir)["hooks"].(map[string]any)
	groups := hooks["SessionStart"].([]any)
	assert.Equal(t, []any{
		map[string]any{"type": "command", "command": userCommand},
		map[string]any{"type": "command", "command": "kata attention-hook start", "timeout": float64(5)},
	}, groups[0].(map[string]any)["hooks"])
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
