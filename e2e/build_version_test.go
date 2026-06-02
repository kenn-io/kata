//go:build !windows

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMakeBuildBakesGitDescribeVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	requireCommand(t, "git")
	requireCommand(t, "make")

	root := repoRoot(t)
	expected := strings.TrimSpace(commandOutput(t, root, "git", "describe", "--tags", "--always", "--dirty"))
	require.NotEmpty(t, expected)

	bin := filepath.Join(root, "kata")
	preserveExistingFile(t, bin)

	_ = commandOutput(t, root, "make", "build")

	out := commandOutput(t, root, bin, "--json", "version")
	var got struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "version output: %s", out)
	assert.Equal(t, expected, got.Version)
}

func TestMakeInstallBakesGitDescribeVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	requireCommand(t, "git")
	requireCommand(t, "make")

	root := repoRoot(t)
	expected := strings.TrimSpace(commandOutput(t, root, "git", "describe", "--tags", "--always", "--dirty"))
	require.NotEmpty(t, expected)

	gobin := t.TempDir()
	_ = commandOutputEnv(t, root, append(os.Environ(), "GOBIN="+gobin), "make", "install")

	out := commandOutput(t, root, filepath.Join(gobin, "kata"), "--json", "version")
	var got struct {
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "version output: %s", out)
	assert.Equal(t, expected, got.Version)
}

func TestMakeBuildSanitizesGitDescribeVersionForShellRecipe(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e")
	}
	requireCommand(t, "make")

	root := repoRoot(t)
	fakeBin := t.TempDir()
	sentinel := filepath.Join(t.TempDir(), "pwned")
	fakeGit := filepath.Join(fakeBin, "git")
	require.NoError(t, os.WriteFile(fakeGit, []byte("#!/usr/bin/env bash\nprintf 'v1.2.3-$(touch "+sentinel+")'\n"), 0o600))
	require.NoError(t, os.Chmod(fakeGit, 0o755)) //nolint:gosec // test needs an executable fake git in a private temp dir.

	out := commandOutputEnv(t, root, append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH")), "make", "-n", "build")

	assert.NotContains(t, out, "$(")
	assert.NotContains(t, out, "`")
	assert.NoFileExists(t, sentinel)
}

func requireCommand(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not found on PATH", name)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(commandOutput(t, ".", "git", "rev-parse", "--show-toplevel"))
}

func commandOutput(t *testing.T, workdir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // test commands are fixed by call sites.
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s %s failed:\n%s", name, strings.Join(args, " "), string(out))
	return string(out)
}

func commandOutputEnv(t *testing.T, workdir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...) //nolint:gosec // test commands are fixed by call sites.
	cmd.Dir = workdir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "%s %s failed:\n%s", name, strings.Join(args, " "), string(out))
	return string(out)
}

func preserveExistingFile(t *testing.T, path string) {
	t.Helper()
	backup := ""
	if _, err := os.Stat(path); err == nil {
		backup = filepath.Join(t.TempDir(), filepath.Base(path))
		require.NoError(t, os.Rename(path, backup))
	} else if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(path)
		if backup != "" {
			_ = os.Rename(backup, path)
		}
	})
}
