package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func installCodexAgentFixture(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(dir, name), "./testdata/codex-agent") //nolint:gosec // Arguments and destination are test controlled.
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KATA_TEST_CODEX_AGENT", mode)
}
