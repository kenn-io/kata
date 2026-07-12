package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// This exercises `kata init --with-hooks` as a user would run it: the real CLI
// command against a live daemon. It complements the focused unit tests in
// init_hooks_test.go.

// TestE2E_InitWithHooks_ComposesWithAgents runs both flags together: guidance
// block and the exact exec-form lifecycle hooks land in one init.
func TestE2E_InitWithHooks_ComposesWithAgents(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/example-org/example-workspace.git")

	out := runCLI(t, env, dir, "init", "--with-agents", "--with-hooks")
	assert.Contains(t, out, "project")

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), agentsBlockBegin)
	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
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
}
