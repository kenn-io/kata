package daemon_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

// setupMockEnv creates a temp dir and points $KATA_HOME and $KATA_DB at it
// for the test's lifetime. Returns the temp dir so callers can construct
// dbhash-derived paths or layer additional env vars on top before calling
// daemon.NewNamespace().
func setupMockEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_DB", filepath.Join(tmp, "kata.db"))
	return tmp
}

func TestNamespace_DataDirIsKataHomeRuntime(t *testing.T) {
	tmp := setupMockEnv(t)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	hash := config.DBHash(filepath.Join(tmp, "kata.db"))
	assert.Equal(t, filepath.Join(tmp, "runtime", hash), ns.DataDir)
	assert.Equal(t, hash, ns.DBHash)
}

func TestNamespaceForName_DataDirUsesNamedDSN(t *testing.T) {
	tmp := setupMockEnv(t)
	t.Setenv("KATA_DB", "")

	ns, err := daemon.NewNamespaceForName("work")
	require.NoError(t, err)
	hash := config.DBHash(filepath.Join(tmp, "kata.work.db"))
	assert.Equal(t, filepath.Join(tmp, "runtime", hash), ns.DataDir)
	assert.Equal(t, hash, ns.DBHash)
}

func TestNamespace_SocketDirHonorsXDGRuntimeDir(t *testing.T) {
	setupMockEnv(t)
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(xdg, "kata", ns.DBHash), ns.SocketDir)
}

func TestNamespace_SocketDirFallsBackToTmpDir(t *testing.T) {
	setupMockEnv(t)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", "/var/folders/xy")

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	assert.Contains(t, ns.SocketDir, "kata-")
	assert.Contains(t, ns.SocketDir, ns.DBHash)
}
