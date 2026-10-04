//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package hooks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoctorHookDiagnosticsRequireWorkingDirectorySearchPermission(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "unsearchable")
	require.NoError(t, os.Mkdir(workdir, 0o600))
	t.Cleanup(func() { _ = os.Chmod(workdir, 0o700) }) //nolint:gosec // G302: restore search permission so temp cleanup can remove the test directory.
	info, err := os.Stat(workdir)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	d, _, _ := mustNewDispatcher(t, []ResolvedHook{{Command: "unused", WorkingDir: workdir}}, defaultConfig())
	got := d.Diagnostics()
	require.Len(t, got.Hooks, 1)
	require.False(t, got.Hooks[0].WorkingDirectoryAvailable)
}
