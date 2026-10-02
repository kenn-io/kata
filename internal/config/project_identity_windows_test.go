//go:build windows

package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/testfix"
)

// TestDiscoverPaths_TraversesJunctionAboveRepo covers a checkout reached
// through a directory junction, such as C:\Users\<user>\code junctioned onto
// another volume. filepath.EvalSymlinks cannot walk through a junction, which
// used to abort discovery with "kata: resolve symlinks <dir>: The system
// cannot find the path specified". Discovery must succeed, and must keep the
// caller's junction-based path the way it keeps a caller's symlinked path.
func TestDiscoverPaths_TraversesJunctionAboveRepo(t *testing.T) {
	target := t.TempDir()
	repo := filepath.Join(target, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755)) //nolint:gosec // test fixture under TempDir.
	testfix.MkDotGit(t, repo)
	workspace := filepath.Join(repo, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o755)) //nolint:gosec // test fixture under TempDir.
	testfix.WriteKataToml(t, workspace, "example-project")

	link := filepath.Join(t.TempDir(), "junction")
	testfix.MkJunction(t, link, target)

	d, err := config.DiscoverPaths(filepath.Join(link, "repo", "workspace"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(link, "repo", "workspace"), d.WorkspaceRoot)
	assert.Equal(t, filepath.Join(link, "repo"), d.GitRoot)
	assert.Equal(t, filepath.Join(link, "repo", "workspace"), config.WriteDestination(d, filepath.Join(link, "repo", "workspace")))
}
