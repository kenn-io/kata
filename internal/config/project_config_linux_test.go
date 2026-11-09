package config_test

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestEnsureProjectConfigHeader_WriteFailurePreservesBinding(t *testing.T) {
	if os.Getenv("KATA_TEST_HEADER_WRITE_FAILURE") == "" {
		//nolint:gosec // Relaunch only this test executable with an isolated file-size limit.
		child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEnsureProjectConfigHeader_WriteFailurePreservesBinding$")
		child.Env = append(os.Environ(), "KATA_TEST_HEADER_WRITE_FAILURE=1")
		output, err := child.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	body := "version = 1\n[project]\nname = \"spoke-project\"\n"
	dir := setupKataProjectDir(t, body)
	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit))
	limit.Cur = uint64(len(body))
	signal.Ignore(syscall.SIGXFSZ)
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit))

	changed, err := config.EnsureProjectConfigHeader(dir)
	require.ErrorIs(t, err, syscall.EFBIG)
	assert.False(t, changed)
	content, err := os.ReadFile(filepath.Join(dir, ".kata.toml")) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, body, string(content))
	cfg, err := config.ReadProjectConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "spoke-project", cfg.Project.Name)
}
