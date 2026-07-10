package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// These exercise `kata init --with-hooks` as a user would run it: the real CLI
// command against a live daemon. They complement the focused unit tests in
// init_hooks_test.go.

// TestE2E_InitWithHooks_NewRepo installs the Claude Code attention hooks in a
// fresh repo: the managed script and the settings.json wiring both appear.
func TestE2E_InitWithHooks_NewRepo(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	out := runCLI(t, env, dir, "init", "--with-hooks")
	assert.Contains(t, out, "project")

	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))
	assert.FileExists(t, filepath.Join(dir, ".claude", "hooks", "kata-attention.sh"))

	settings, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(settings), "kata-attention.sh claim")
	assert.Contains(t, string(settings), "kata-attention.sh stop")

	// --with-hooks does not drag in the guidance block.
	assert.NoFileExists(t, filepath.Join(dir, "AGENTS.md"))
}

// TestE2E_InitWithHooks_ComposesWithAgents runs both flags together: guidance
// block and hooks land in one init.
func TestE2E_InitWithHooks_ComposesWithAgents(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	runCLI(t, env, dir, "init", "--with-agents", "--with-hooks")

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), agentsBlockBegin)
	assert.FileExists(t, filepath.Join(dir, ".claude", "hooks", "kata-attention.sh"))
}
