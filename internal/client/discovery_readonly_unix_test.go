//go:build !windows

package client

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeRuntimeDirectoryInsecureForTest(t *testing.T, dir string) func() {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o755)) //nolint:gosec // G302: exercise discovery of a directory with unsafe permissions.
	return func() {
		t.Helper()
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "read-only discovery must not repair permissions")
	}
}
