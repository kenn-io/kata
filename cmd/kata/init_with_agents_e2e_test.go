package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// These exercise `kata init --with-agents` as a user would run it: the real
// CLI command against a live daemon, in three realistic shapes — a brand-new
// repo, an existing repo that already keeps agent docs, and a migration from
// Beads. They complement the focused unit tests in init_test.go.

// TestE2E_InitWithAgents_NewEmptyRepo is the happy path: a fresh git repo with
// no agent docs. init binds the project and writes AGENTS.md, and does not
// fabricate a CLAUDE.md.
func TestE2E_InitWithAgents_NewEmptyRepo(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	out := runCLI(t, env, dir, "init", "--with-agents")
	assert.Contains(t, out, "project")

	assert.FileExists(t, filepath.Join(dir, ".kata.toml"))

	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), agentsBlockBegin)
	assert.Contains(t, string(content), agentsBlockEnd)
	assert.Contains(t, string(content), "kata quickstart")

	// kata writes AGENTS.md only; it does not invent a CLAUDE.md.
	assert.NoFileExists(t, filepath.Join(dir, "CLAUDE.md"))
}

// TestE2E_InitWithAgents_PreservesExistingAgentDocs runs init in a repo that
// already ships a CLAUDE.md and an AGENTS.md full of unrelated guidance. The
// CLAUDE.md must be byte-for-byte untouched, and every pre-existing AGENTS.md
// section must survive alongside kata's appended block.
func TestE2E_InitWithAgents_PreservesExistingAgentDocs(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	claude := "# Claude guidance\n\nProject-specific Claude rules. Leave this file alone.\n"
	agents := "# Agent guidance\n\n## Build\n\nRun `make build` before pushing.\n\n## Conventions\n\nTabs, not spaces.\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(claude), 0o644)) //nolint:gosec // test fixture under TempDir
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agents), 0o644)) //nolint:gosec // test fixture under TempDir

	runCLI(t, env, dir, "init", "--with-agents")

	// CLAUDE.md is not part of the managed surface — it must be identical.
	gotClaude, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, claude, string(gotClaude), "CLAUDE.md must be left exactly as the project had it")

	// AGENTS.md keeps every pre-existing section and gains kata's block.
	gotAgents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	got := string(gotAgents)
	assert.Contains(t, got, "# Agent guidance")
	assert.Contains(t, got, "Run `make build` before pushing.")
	assert.Contains(t, got, "Tabs, not spaces.")
	assert.Contains(t, got, agentsBlockBegin)
	assert.Contains(t, got, "kata quickstart")
}

// TestE2E_InitWithAgents_ThenBeadsImport walks the realistic migration: a repo
// coming from Beads (which already wrote its own AGENTS.md) runs
// `kata init --with-agents` and then `kata import --source-format beads`. The
// issues import and the AGENTS.md retains both the legacy content and kata's
// block through the whole flow.
func TestE2E_InitWithAgents_ThenBeadsImport(t *testing.T) {
	resetFlags(t)
	env := testenv.New(t)
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "remote", "add", "origin", "https://github.com/wesm/kata.git")

	// A project migrating from Beads commonly already keeps an AGENTS.md.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENTS.md"), //nolint:gosec // test fixture under TempDir
		[]byte("# Agent guidance\n\nLegacy notes from the Beads era.\n"), 0o644))

	installFakeBD(t)

	runCLI(t, env, dir, "init", "--with-agents")

	out, err := runCLICapture(t, env, dir, "import", "--source-format", "beads", "--as", "importer")
	require.NoError(t, err, "import output: %s", out)
	assert.Contains(t, out, "imported beads: created 1")

	// The imported issue is queryable through the normal read path.
	listOut := runCLI(t, env, dir, "list", "--json")
	assert.Contains(t, listOut, "Live bead")

	// AGENTS.md survived the migration: legacy content + kata block coexist.
	content, err := os.ReadFile(filepath.Join(dir, "AGENTS.md")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Contains(t, string(content), "Legacy notes from the Beads era.")
	assert.Contains(t, string(content), agentsBlockBegin)
	assert.Contains(t, string(content), "kata quickstart")
}
