package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stubDaemonExecutable(t *testing.T, resolve func() (string, error)) {
	t.Helper()
	original := resolveDaemonExecutable
	resolveDaemonExecutable = resolve
	t.Cleanup(func() { resolveDaemonExecutable = original })
}

func TestNewDaemonRestart_UnresolvableExecutableDisablesRestart(t *testing.T) {
	stubDaemonExecutable(t, func() (string, error) { return "", errors.New("procfs unavailable") })
	var stderr bytes.Buffer
	restart := newDaemonRestart(&stderr)
	assert.Nil(t, restart, "startup must continue without automatic restart")
	assert.Contains(t, stderr.String(), "procfs unavailable")
	assert.Contains(t, stderr.String(), "automatic restart")
}

func TestNewDaemonRestart_MissingExecutableDisablesRestart(t *testing.T) {
	stubDaemonExecutable(t, func() (string, error) { return filepath.Join(t.TempDir(), "kata"), nil })
	var stderr bytes.Buffer
	assert.Nil(t, newDaemonRestart(&stderr))
	assert.Contains(t, stderr.String(), "automatic restart")
}

func TestNewDaemonRestart_EphemeralExecutableIsSilent(t *testing.T) {
	stubDaemonExecutable(t, os.Executable)
	var stderr bytes.Buffer
	assert.Nil(t, newDaemonRestart(&stderr), "test binaries never re-execute")
	assert.Empty(t, stderr.String())
}

func TestNewDaemonRestart_ResolvesSymlinkedExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "kata")
	require.NoError(t, os.WriteFile(target, []byte("#!/bin/sh\n"), 0o600))
	link := filepath.Join(dir, "kata-link")
	require.NoError(t, os.Symlink(target, link))
	stubDaemonExecutable(t, func() (string, error) { return link, nil })
	var stderr bytes.Buffer
	restart := newDaemonRestart(&stderr)
	require.NotNil(t, restart)
	// Windows temp dirs may carry 8.3 short names; resolve the expected path the same way.
	expected, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Equal(t, expected, restart.executable)
	assert.Empty(t, stderr.String())
}
