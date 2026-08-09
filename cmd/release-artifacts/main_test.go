package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_RequiresKnownSubcommand(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "build-source")
		assert.Contains(t, err.Error(), "verify-source")
		assert.Contains(t, err.Error(), "render-homebrew-core")
	}
}

func TestRun_BuildSourceRequiresVersionOutputAndMetadata(t *testing.T) {
	for _, args := range [][]string{
		{"build-source"},
		{"build-source", "--version", "0.14.2"},
		{"build-source", "--version", "0.14.2", "--output", "source.tar.gz"},
	} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, &stdout, &stderr)
		require.Error(t, err)
	}
}

func TestRun_BuildSourceSnapshotWritesArchiveAndMetadata(t *testing.T) {
	repo := t.TempDir()
	runReleaseGit(t, repo, "init", "-q")
	runReleaseGit(t, repo, "config", "user.name", "Example User")
	runReleaseGit(t, repo, "config", "user.email", "example@example.test")
	goMod := `module go.kenn.io/kata

go 1.26.3

require example.test/dependency v0.0.0

replace example.test/dependency => ./third_party/dependency
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte(goMod), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "fixture.go"), []byte("package kata\n\nimport _ \"example.test/dependency\"\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "third_party", "dependency"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "third_party", "dependency", "go.mod"), []byte("module example.test/dependency\n\ngo 1.26.3\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "third_party", "dependency", "dependency.go"), []byte("package dependency\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "internal/web/dist"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("snapshot\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "internal/web/dist/index.html"), []byte("production-assets\n"), 0o600))
	runReleaseGit(t, repo, "add", ".")
	runReleaseGit(t, repo, "commit", "-m", "snapshot fixture")
	archive := filepath.Join(t.TempDir(), "kata_snapshot_source.tar.gz")
	metadata := filepath.Join(t.TempDir(), "source-archive.json")
	var stdout, stderr bytes.Buffer

	err := run(context.Background(), []string{
		"build-source", "--repo", repo, "--version", "0.14.2-SNAPSHOT", "--snapshot",
		"--output", archive, "--metadata", metadata,
	}, &stdout, &stderr)

	require.NoError(t, err, stderr.String())
	assert.FileExists(t, archive)
	assert.FileExists(t, metadata)
}

func runReleaseGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // fixed test helper arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
