// Package testfix provides shared filesystem and git fixtures and a
// schema-ready SQLite template for tests.
package testfix

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	gitcmd "go.kenn.io/kit/git/cmd"
)

// WriteKataToml writes a minimal v1 .kata.toml under dir with the given name.
// The world-readable file mode mirrors how users commit .kata.toml in real
// projects.
func WriteKataToml(t *testing.T, dir, name string) {
	t.Helper()
	body := "version = 1\n\n[project]\nname = " + strconv.Quote(name) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte(body), 0o644)) //nolint:gosec // test fixture mirrors production .kata.toml mode
}

// MkDotGit creates an empty .git directory under dir so that walk-up
// discovery code can detect a git workspace without a real repository.
func MkDotGit(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755)) //nolint:gosec // test fixture under TempDir
}

// InitGitRepo creates a fresh temp directory, runs git init, and configures
// a deterministic author so commit-producing tests don't depend on the host
// git config. Returns the repo path.
func InitGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	RunGit(t, dir, "init", "--quiet")
	RunGit(t, dir, "config", "user.email", "x@example.com")
	RunGit(t, dir, "config", "user.name", "x")
	return dir
}

// RunGit invokes the system git binary inside dir with args and fails the
// test, surfacing combined output, if the command errors.
func RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	stdout, stderr, err := gitcmd.New().Run(t.Context(), dir, nil, args...)
	require.NoErrorf(t, err, "git %v: %s%s", args, stdout, stderr)
}
