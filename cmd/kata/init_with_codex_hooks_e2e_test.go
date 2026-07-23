package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// This exercises `kata init --with-codex-hooks` as a user would run it: the
// real CLI command against a live daemon. It complements the focused unit
// tests in init_hooks_codex_test.go.

// TestE2E_InitWithCodexHooks installs the Codex SessionStart wiring on its own.
func TestE2E_InitWithCodexHooks(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")

	out := runCLI(t, env, dir, "init", "--with-codex-hooks")
	assert.Contains(t, out, "project")

	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{expectedCodexHandler()},
			}},
		},
	}, readCodexHooks(t, dir))
}

// TestE2E_InitWithCodexHooks_ComposesWithAgentsAndHooks runs all three
// workspace-wiring flags together: guidance block, Claude Code hooks, and
// Codex hooks all land in one init, and a second identical init is a no-op.
func TestE2E_InitWithCodexHooks_ComposesWithAgentsAndHooks(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")

	runCLI(t, env, dir, "init", "--with-agents", "--with-hooks", "--with-codex-hooks")

	// All three artifacts exist.
	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), agentsBlockBegin)

	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"matcher": "startup|resume|clear",
				"hooks":   []any{expectedHookHandler("start")},
			}},
			"SessionEnd": []any{map[string]any{
				"matcher": "logout|prompt_input_exit|bypass_permissions_disabled|other",
				"hooks":   []any{expectedHookHandler("end")},
			}},
		},
	}, readSettings(t, dir))

	codexPath := filepath.Join(dir, ".codex", "hooks.json")
	assert.Equal(t, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{map[string]any{
				"hooks": []any{expectedCodexHandler()},
			}},
		},
	}, readCodexHooks(t, dir))

	// Re-running the same init leaves the Codex hooks byte-for-byte unchanged.
	before, err := os.ReadFile(codexPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	runCLI(t, env, dir, "init", "--with-agents", "--with-hooks", "--with-codex-hooks")
	after, err := os.ReadFile(codexPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after)
}
