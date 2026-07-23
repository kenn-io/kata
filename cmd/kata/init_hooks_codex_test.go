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

// Unit tests for `kata init --with-codex-hooks`: merging the work.attention
// SessionStart wiring into .codex/hooks.json. Codex runs `command` through a
// shell, so the whole invocation is one string (contrast with Claude Code's
// separate command+args exec form).

func readCodexHooks(t *testing.T, dir string) map[string]any {
	t.Helper()
	bs, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	var file map[string]any
	decoder := json.NewDecoder(bytes.NewReader(bs))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&file))
	return file
}

func writeCodexHooks(t *testing.T, dir string, file map[string]any) string {
	t.Helper()
	bs, err := json.Marshal(file)
	require.NoError(t, err)
	return writeRawCodexHooks(t, dir, string(bs))
}

func writeRawCodexHooks(t *testing.T, dir, content string) string {
	t.Helper()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	path := filepath.Join(codexDir, "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644)) //nolint:gosec // test fixture under TempDir
	return path
}

func writeCodexConfig(t *testing.T, dir, content string) {
	t.Helper()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(content), 0o644)) //nolint:gosec // test fixture under TempDir
}

// expectedCodexHandler is the single managed command hook. timeout decodes to
// json.Number under UseNumber, matching how the writer stores it.
func expectedCodexHandler() map[string]any {
	return map[string]any{
		"type":    "command",
		"command": "kata attention-hook start",
		"timeout": json.Number("10"),
	}
}

func TestApplyCodexHooks_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, warnings)

	// The managed SessionStart group covers real session transitions without
	// resetting attention during context compaction.
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup|resume|clear",
				"hooks":   []any{expectedCodexHandler()},
			}},
		},
	}, readCodexHooks(t, dir))
}

func TestApplyCodexHooks_Idempotent(t *testing.T) {
	dir := t.TempDir()

	changed, _, err := applyCodexHooks(dir)
	require.NoError(t, err)
	require.True(t, changed)

	path := filepath.Join(dir, ".codex", "hooks.json")
	before, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.False(t, changed, "an exact installation must be a no-op")
	assert.Empty(t, warnings)

	after, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

func TestApplyCodexHooks_NarrowsLegacyMatcherlessHook(t *testing.T) {
	dir := t.TempDir()
	writeCodexHooks(t, dir, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{expectedCodexHandler()},
			}},
		},
	})

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, warnings)
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup|resume|clear",
				"hooks":   []any{expectedCodexHandler()},
			}},
		},
	}, readCodexHooks(t, dir))

	changed, warnings, err = applyCodexHooks(dir)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, warnings)
}

func TestApplyCodexHooks_PreservesExistingContent(t *testing.T) {
	dir := t.TempDir()

	// A description, an unrelated event group, and a matcher-scoped
	// SessionStart group the managed wiring must not touch.
	preTool := []any{map[string]any{
		"hooks": []any{map[string]any{"type": "command", "command": "lint"}},
	}}
	scopedStart := map[string]any{
		"matcher": "startup",
		"hooks":   []any{map[string]any{"type": "command", "command": "notify"}},
	}
	writeCodexHooks(t, dir, map[string]any{
		"description": "project hooks",
		"hooks": map[string]any{
			"PreToolUse":   preTool,
			"SessionStart": []any{scopedStart},
		},
	})

	changed, warnings, err := applyCodexHooks(dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, warnings)

	file := readCodexHooks(t, dir)
	assert.Equal(t, "project hooks", file["description"])
	hooks := file["hooks"].(map[string]any)
	assert.Equal(t, preTool, hooks["PreToolUse"])

	// The narrower matcher-scoped group does not cover the full managed wiring,
	// so it is preserved and the managed group is appended after it.
	assert.Equal(t, []any{
		scopedStart,
		map[string]any{
			"matcher": "startup|resume|clear",
			"hooks":   []any{expectedCodexHandler()},
		},
	}, hooks["SessionStart"])
}

func TestApplyCodexHooks_RejectsMalformedWithoutWriting(t *testing.T) {
	tests := map[string]string{
		"invalid JSON":        "{ not json",
		"multiple roots":      "{} {}",
		"null root":           "null\n",
		"array root":          "[]\n",
		"null hooks":          `{"hooks":null}`,
		"array hooks":         `{"hooks":[]}`,
		"object SessionStart": `{"hooks":{"SessionStart":{}}}`,
		"string SessionStart": `{"hooks":{"SessionStart":"broken"}}`,
	}
	for name, original := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeRawCodexHooks(t, dir, original)

			_, _, err := applyCodexHooks(dir)
			require.Error(t, err)

			got, readErr := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
			require.NoError(t, readErr)
			assert.Equal(t, original, string(got))
		})
	}
}

func TestApplyCodexHooks_RefusesSymlinks(t *testing.T) {
	t.Run(".codex directory", func(t *testing.T) {
		dir := t.TempDir()
		outside := t.TempDir()
		require.NoError(t, os.Symlink(outside, filepath.Join(dir, ".codex")))

		_, _, err := applyCodexHooks(dir)
		require.Error(t, err)

		entries, readErr := os.ReadDir(outside)
		require.NoError(t, readErr)
		assert.Empty(t, entries)
	})

	t.Run("hooks.json", func(t *testing.T) {
		dir := t.TempDir()
		codexDir := filepath.Join(dir, ".codex")
		require.NoError(t, os.MkdirAll(codexDir, 0o750))
		victim := filepath.Join(dir, "victim.json")
		require.NoError(t, os.WriteFile(victim, []byte("{}"), 0o644)) //nolint:gosec // test fixture under TempDir
		require.NoError(t, os.Symlink(victim, filepath.Join(codexDir, "hooks.json")))

		_, _, err := applyCodexHooks(dir)
		require.Error(t, err)

		got, readErr := os.ReadFile(victim) //nolint:gosec // test fixture under TempDir
		require.NoError(t, readErr)
		assert.Equal(t, "{}", string(got))
	})
}

func TestApplyCodexHooks_WarnsOnConfigTomlHooks(t *testing.T) {
	t.Run("config.toml with [hooks] warns and still installs", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, "[hooks]\nSessionStart = []\n")

		changed, warnings, err := applyCodexHooks(dir)
		require.NoError(t, err)
		assert.True(t, changed)
		require.NotEmpty(t, warnings)

		assert.Equal(t, map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{map[string]any{
					"matcher": "startup|resume|clear",
					"hooks":   []any{expectedCodexHandler()},
				}},
			},
		}, readCodexHooks(t, dir))
	})

	t.Run("config.toml without [hooks] does not warn", func(t *testing.T) {
		dir := t.TempDir()
		writeCodexConfig(t, dir, "model = \"gpt-5\"\n")

		changed, warnings, err := applyCodexHooks(dir)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Empty(t, warnings)
	})
}
