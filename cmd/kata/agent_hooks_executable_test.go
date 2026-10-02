package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/fslink"
)

func TestAgentHookExecutableResolution(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "versioned-kata")
	require.NoError(t, os.WriteFile(running, []byte("fixture"), 0o700)) //nolint:gosec // G306: owner-only executable fixture for PATH selection.
	first, second := filepath.Join(dir, "first"), filepath.Join(dir, "second")
	require.NoError(t, os.Mkdir(first, 0o700))
	require.NoError(t, os.Mkdir(second, 0o700))
	name := "kata"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(first, name), []byte("other"), 0o700)) //nolint:gosec // G306: owner-only executable fixture for PATH selection.
	shim := filepath.Join(second, name)
	if err := os.Symlink(running, shim); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	env := agentHookExecutableEnv{Executable: func() (string, error) { return running, nil }, EvalSymlinks: filepath.EvalSymlinks, Path: first + string(os.PathListSeparator) + second, GOOS: runtime.GOOS}
	got, warning, err := resolveAgentHookExecutable("", env)
	require.NoError(t, err)
	require.Equal(t, shim, got)
	require.Empty(t, warning)
	override := filepath.Join(dir, "explicit shim.exe")
	require.NoError(t, os.WriteFile(override, []byte("fixture"), 0o700)) //nolint:gosec // G306: executable fixture.
	got, warning, err = resolveAgentHookExecutable(override, env)
	require.NoError(t, err)
	require.Equal(t, override, got)
	require.Empty(t, warning)
	env.Path = first
	got, warning, err = resolveAgentHookExecutable("", env)
	require.NoError(t, err)
	require.Equal(t, running, got)
	require.Contains(t, warning, "upgrade")
	env.Executable = func() (string, error) { return "", errors.New("executable failed") }
	_, _, err = resolveAgentHookExecutable("", env)
	require.ErrorContains(t, err, "executable failed")
	got, _, err = resolveAgentHookExecutable(override, env)
	require.NoError(t, err)
	require.Equal(t, override, got)
}

func TestDefaultAgentHookExecutableEnvPreservesStableDirectoryAlias(t *testing.T) {
	root := t.TempDir()
	versioned := filepath.Join(root, "versioned")
	stable := filepath.Join(root, "stable")
	require.NoError(t, os.Mkdir(versioned, 0700))
	name := "kata"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	running := filepath.Join(versioned, name)
	require.NoError(t, os.WriteFile(running, []byte("fixture"), 0700)) //nolint:gosec // G306: isolated owner-only executable fixture for PATH selection.
	if runtime.GOOS == "windows" {
		if err := fslink.CreateJunction(versioned, stable); err != nil {
			t.Skipf("junction unavailable: %v", err)
		}
	} else {
		require.NoError(t, os.Symlink(versioned, stable))
	}
	t.Setenv("PATH", stable)
	env := defaultAgentHookExecutableEnv()
	env.Executable = func() (string, error) { return running, nil }
	got, warning, err := resolveAgentHookExecutable("", env)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(stable, name), got)
	require.Empty(t, warning)
}

func TestAgentHookExecutableCommandParsing(t *testing.T) {
	for _, path := range []string{"/tmp/example/bin/kata", "/tmp/example path/kata", "/tmp/example's path/kata", `C:\Program Files\example\kata.exe`, `C:\example\quoted"kata.exe`, `C:\example path\`} {
		commands, err := agenthook.BuildCommand(path, "agent-hooks", "contract", "codex", "--source", legacyAgentContractHookSource)
		require.NoError(t, err)
		for _, tc := range []struct {
			command, goos string
			powershell    bool
		}{
			{commands.POSIX, "linux", false}, {commands.Windows, "windows", false}, {commands.PowerShell, "windows", true},
		} {
			got, err := agentHookCommandExecutable(tc.command, tc.goos, tc.powershell)
			require.NoError(t, err, tc.command)
			require.Equal(t, path, got, tc.command)
		}
	}
	for _, command := range []string{"", "'unterminated", `"unterminated`} {
		_, err := agentHookCommandExecutable(command, "linux", false)
		require.Error(t, err)
	}
}
