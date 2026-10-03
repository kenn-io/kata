//go:build !windows

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeDoctorRuntimeDirectoryInsecureForTest(t *testing.T, dir string) func() {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // G302: exercise doctor with unsafe runtime directory permissions.
	return func() {
		t.Helper()
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "doctor must not repair runtime permissions")
	}
}
