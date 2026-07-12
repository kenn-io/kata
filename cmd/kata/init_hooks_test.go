package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests for `kata init --with-hooks`: merging the work.attention
// lifecycle wiring into .claude/settings.json. The hooks call the installed
// kata binary directly through Claude Code's exec form.

func readSettings(t *testing.T, dir string) map[string]any {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	var settings map[string]any
	decoder := json.NewDecoder(bytes.NewReader(bs))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&settings))
	return settings
}

func writeSettings(t *testing.T, dir string, settings map[string]any) string {
	t.Helper()
	bs, err := json.Marshal(settings)
	require.NoError(t, err)
	return writeRawSettings(t, dir, string(bs))
}

func writeRawSettings(t *testing.T, dir, content string) string {
	t.Helper()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o750))
	path := filepath.Join(claudeDir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644)) //nolint:gosec // test fixture under TempDir
	return path
}

func expectedHookHandler(mode string) map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata",
		"args":    []any{"attention-hook", mode},
	}
}

func TestApplyClaudeHooks_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)

	// settings.json contains exactly the two managed lifecycle hooks. Claude
	// Code's exec form keeps the binary and arguments separate, with no shell
	// command string and no PostToolUse wiring.
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup|resume|clear",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "kata",
					"args":    []any{"attention-hook", "start"},
				}},
			}},
			"SessionEnd": []any{map[string]any{
				"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
				"hooks": []any{map[string]any{
					"type":    "command",
					"command": "kata",
					"args":    []any{"attention-hook", "end"},
				}},
			}},
		},
	}, readSettings(t, dir))
}

func TestApplyClaudeHooks_PreservesSettingsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	unscopedStartGroup := map[string]any{
		"custom": "keep",
		"hooks": []any{
			map[string]any{"type": "command", "command": "notify-start"},
			expectedHookHandler("start"),
		},
	}
	unscopedEndGroup := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": "notify-end"},
			expectedHookHandler("end"),
		},
	}
	postToolUse := []any{map[string]any{
		"matcher": "Bash",
		"hooks":   []any{map[string]any{"type": "command", "command": "lint"}},
	}}
	stop := []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "notify-stop"}},
	}}
	writeSettings(t, dir, map[string]any{
		"permissions":  map[string]any{"allow": []any{"WebFetch"}},
		"futureKey":    map[string]any{"enabled": true},
		"largeInteger": json.Number("9007199254740993"),
		"hooks": map[string]any{
			"SessionStart": []any{unscopedStartGroup},
			"SessionEnd":   []any{unscopedEndGroup},
			"PostToolUse":  postToolUse,
			"Stop":         stop,
		},
	})

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)

	settings := readSettings(t, dir)
	assert.Equal(t, map[string]any{"allow": []any{"WebFetch"}}, settings["permissions"])
	assert.Equal(t, map[string]any{"enabled": true}, settings["futureKey"])
	assert.Equal(t, json.Number("9007199254740993"), settings["largeInteger"])
	hooks := settings["hooks"].(map[string]any)
	assert.Equal(t, postToolUse, hooks["PostToolUse"])
	assert.Equal(t, stop, hooks["Stop"])

	// Unscoped lifecycle handlers do not cover the exact managed matchers.
	// Preserve their shared groups and add the two managed scoped groups.
	assert.Equal(t, []any{
		unscopedStartGroup,
		map[string]any{
			"matcher": "startup|resume|clear",
			"hooks":   []any{expectedHookHandler("start")},
		},
	}, hooks["SessionStart"])
	assert.Equal(t, []any{
		unscopedEndGroup,
		map[string]any{
			"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
			"hooks":   []any{expectedHookHandler("end")},
		},
	}, hooks["SessionEnd"])

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	before, err := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	changed, err = applyClaudeHooks(dir)
	require.NoError(t, err)
	assert.False(t, changed, "an exact installation must be a no-op")
	after, err := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestApplyClaudeHooks_RejectsMalformedSettingsWithoutWriting(t *testing.T) {
	tests := map[string]string{
		"invalid JSON":             "{ not json",
		"multiple roots":           "{} {}",
		"null root":                "null\n",
		"array root":               "[]\n",
		"null hooks":               `{"hooks":null}`,
		"array hooks":              `{"hooks":[]}`,
		"null SessionStart":        `{"hooks":{"SessionStart":null}}`,
		"object SessionStart":      `{"hooks":{"SessionStart":{}}}`,
		"string SessionEnd":        `{"hooks":{"SessionEnd":"broken"}}`,
		"valid start, invalid end": `{"hooks":{"SessionStart":[],"SessionEnd":null}}`,
	}
	for name, original := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			settingsPath := writeRawSettings(t, dir, original)

			_, err := applyClaudeHooks(dir)
			require.Error(t, err)

			got, readErr := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
			require.NoError(t, readErr)
			assert.Equal(t, original, string(got))
		})
	}
}

func TestApplyClaudeHooks_RefusesSymlinks(t *testing.T) {
	t.Run(".claude directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, ".claude")))

		_, err := applyClaudeHooks(dir)
		require.Error(t, err)

		entries, readErr := os.ReadDir(outside)
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	})

	t.Run("settings.json", func(t *testing.T) {
		dir := t.TempDir()
		claudeDir := filepath.Join(dir, ".claude")
		require.NoError(t, os.MkdirAll(claudeDir, 0o750))
		victim := filepath.Join(dir, "victim.json")
		require.NoError(t, os.WriteFile(victim, []byte("{}"), 0o644)) //nolint:gosec // test fixture under TempDir
		require.NoError(t, os.Symlink(victim, filepath.Join(claudeDir, "settings.json")))

		_, err := applyClaudeHooks(dir)
		require.Error(t, err)

		got, readErr := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
		require.NoError(t, readErr)
		assert.Equal(t, "{}", string(got))
	})
}

func TestApplyClaudeHooks_AtomicRewritePreservesOriginalOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not make a directory unwritable for file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	settingsPath := writeRawSettings(t, dir, "{\n  \"permissions\": {}\n}\n")
	original, err := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	require.NoError(t, os.Chmod(claudeDir, 0o500))       //nolint:gosec // test fixture under TempDir
	t.Cleanup(func() { _ = os.Chmod(claudeDir, 0o750) }) //nolint:gosec // restore for TempDir cleanup

	_, err = applyClaudeHooks(dir)
	require.Error(t, err)
	got, readErr := os.ReadFile(settingsPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, original, got)
}

func TestApplyClaudeHooks_AtomicRewritePreservesMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix file mode bits are not represented on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reports unrestricted file modes")
	}
	dir := t.TempDir()
	settingsPath := writeRawSettings(t, dir, "{}\n")
	require.NoError(t, os.Chmod(settingsPath, 0o640)) //nolint:gosec // test fixture under TempDir

	changed, err := applyClaudeHooks(dir)
	require.NoError(t, err)
	require.True(t, changed)

	info, err := os.Stat(settingsPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(settingsPath))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "settings.json", entries[0].Name())
}
