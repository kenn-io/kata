package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
)

func TestAllowsNewPostgresMigration(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	writeFile(t, repo, "internal/db/pgstore/migrations/000002_next.up.sql", "new\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000002_next.up.sql")

	var stderr bytes.Buffer
	assert.Zero(t, run(t.Context(), &stderr))
	assert.Empty(t, stderr.String())
}

func TestAllowsMigrationDocumentationEdit(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	gitCommand(t, "checkout", "main")
	writeFile(t, repo, "internal/db/pgstore/migrations/README.md", "old docs\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/README.md")
	gitCommand(t, "commit", "-qm", "add docs")
	gitCommand(t, "checkout", "feature")
	writeFile(t, repo, "internal/db/pgstore/migrations/README.md", "new docs\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/README.md")

	var stderr bytes.Buffer
	assert.Zero(t, run(t.Context(), &stderr))
	assert.Empty(t, stderr.String())
}

func TestBlocksDuplicatePostgresMigrationVersion(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	gitCommand(t, "checkout", "main")
	writeFile(t, repo, "internal/db/pgstore/migrations/000002_main_name.up.sql", "main\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000002_main_name.up.sql")
	gitCommand(t, "commit", "-qm", "add main migration")
	gitCommand(t, "checkout", "feature")
	writeFile(t, repo, "internal/db/pgstore/migrations/000002_branch_name.up.sql", "branch\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000002_branch_name.up.sql")

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "duplicate migration version")
	assert.Contains(t, stderr.String(), "000002")
	assert.Contains(t, stderr.String(), "000002_branch_name")
	assert.Contains(t, stderr.String(), "000002_main_name")
}

func TestBlocksDuplicateVersionAcrossFeatureBranchCommits(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	writeFile(t, repo, "internal/db/pgstore/migrations/000002_first_branch_name.up.sql", "first\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000002_first_branch_name.up.sql")
	gitCommand(t, "commit", "-qm", "add first branch migration")
	writeFile(t, repo, "internal/db/pgstore/migrations/000002_second_branch_name.up.sql", "second\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000002_second_branch_name.up.sql")

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "duplicate migration version")
	assert.Contains(t, stderr.String(), "000002_first_branch_name")
	assert.Contains(t, stderr.String(), "000002_second_branch_name")
}

func TestBlocksInvalidPostgresMigrationName(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	writeFile(t, repo, "internal/db/pgstore/migrations/2_quick_fix.up.sql", "new\n")
	writeFile(t, repo, "internal/db/pgstore/migrations/000002_quick_fix.down.sql", "down\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations")

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "Invalid migration filenames")
	assert.Contains(t, stderr.String(), "000002_quick_fix.down.sql")
	assert.Contains(t, stderr.String(), "2_quick_fix.up.sql")
}

func TestBlocksReleasedPostgresMigrationEdit(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	writeFile(t, repo, "internal/db/pgstore/migrations/000001_init.up.sql", "changed\n")
	gitCommand(t, "add", "internal/db/pgstore/migrations/000001_init.up.sql")

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "Refusing to commit staged PostgreSQL migration history changes")
	assert.Contains(t, stderr.String(), "internal/db/pgstore/migrations/000001_init.up.sql")
}

func TestBlocksReleasedPostgresMigrationRename(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	gitCommand(t, "mv", "internal/db/pgstore/migrations/000001_init.up.sql", "internal/db/pgstore/migrations/000001_renamed.up.sql")

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "internal/db/pgstore/migrations/000001_init.up.sql")
}

func TestUsesHookGitIndexFile(t *testing.T) {
	isolateGitEnvironment(t)
	repo := initRepoWithMainMigration(t)
	t.Chdir(repo)
	t.Setenv("KATA_MIGRATION_BASE_REF", "main")

	alternateIndex := filepath.Join(t.TempDir(), "index")
	hookEnv := append(cleanGitEnv(os.Environ()), "GIT_INDEX_FILE="+alternateIndex)
	gitCommandInWithEnv(t, repo, hookEnv, "read-tree", "HEAD")

	writeFile(t, repo, "internal/db/pgstore/migrations/000001_init.up.sql", "changed in hook index\n")
	gitCommandInWithEnv(t, repo, hookEnv, "add", "internal/db/pgstore/migrations/000001_init.up.sql")

	originalGitEnv := gitEnv
	gitEnv = hookEnv
	t.Cleanup(func() {
		gitEnv = originalGitEnv
	})

	var stderr bytes.Buffer
	assert.Equal(t, 1, run(t.Context(), &stderr))
	assert.Contains(t, stderr.String(), "Refusing to commit staged PostgreSQL migration history changes")
	assert.Contains(t, stderr.String(), "internal/db/pgstore/migrations/000001_init.up.sql")
}

func initRepoWithMainMigration(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	migrationPath := filepath.Join(repo, "internal/db/pgstore/migrations/000001_init.up.sql")
	require.NoError(t, os.MkdirAll(filepath.Dir(migrationPath), 0o750))
	require.NoError(t, os.WriteFile(migrationPath, []byte("old\n"), 0o600))

	gitCommandIn(t, repo, "init", "-q", "-b", "main")
	gitCommandIn(t, repo, "config", "user.email", "kata-fixture@example.invalid")
	gitCommandIn(t, repo, "config", "user.name", "Test")
	gitCommandIn(t, repo, "add", ".")
	gitCommandIn(t, repo, "commit", "-qm", "init")
	gitCommandIn(t, repo, "checkout", "-qb", "feature")

	return repo
}

func writeFile(t *testing.T, repo, path, content string) {
	t.Helper()

	fullPath := filepath.Join(repo, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o750))
	require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o600))
}

func gitCommand(t *testing.T, args ...string) {
	t.Helper()
	gitCommandIn(t, "", args...)
}

func gitCommandIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitCommandInWithEnv(t, dir, cleanGitEnv(os.Environ()), args...)
}

func gitCommandInWithEnv(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()

	runner := gitcmd.New().WithConfig("core.hooksPath", os.DevNull)
	runner.Env = env
	runner.StripEnv = false
	output, _, err := runner.Run(t.Context(), dir, nil, args...)
	require.NoError(t, err, string(output))
}

func isolateGitEnvironment(t *testing.T) {
	t.Helper()

	originalGitEnv := gitEnv
	gitEnv = cleanGitEnv(os.Environ())
	t.Cleanup(func() {
		gitEnv = originalGitEnv
	})
}

func cleanGitEnv(env []string) []string {
	return gitenv.StripAll(env)
}
