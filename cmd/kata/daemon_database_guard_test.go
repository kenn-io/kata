package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	kitdaemon "go.kenn.io/kit/daemon"
)

// Contract vmr7: a live daemon from another version must prevent opening or
// migrating its database even when its listener differs from the requested one.
func TestDaemonRefusesCrossVersionRuntimeBeforeMigration(t *testing.T) {
	home := setupKataEnv(t)
	path := filepath.Join(home, "kata.db")
	s, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = s.ExecContext(t.Context(), `UPDATE meta SET value='1' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Version: "v0.18.0", Network: "tcp", Address: "127.0.0.1:12345",
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = runDaemonWithListen(ctx, "127.0.0.1:0", false)
	require.ErrorContains(t, err, "daemon already running")
	ver, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, 1, ver)
}

func TestDaemonRefusesLegacyRuntimeRecordAcrossSQLiteSymlink(t *testing.T) {
	home := setupKataEnv(t)
	path := filepath.Join(home, "kata.db")
	s, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	_, err = s.ExecContext(t.Context(), `UPDATE meta SET value='1' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	ns, err := daemon.NewNamespace()
	require.NoError(t, err)
	require.NoError(t, ns.EnsureDirs())
	_, err = (kitdaemon.RuntimeStore{Dir: ns.DataDir}).Write(kitdaemon.RuntimeRecord{
		PID: os.Getpid(), Version: "v0.18.0", Network: "tcp", Address: "127.0.0.1:12345",
		Metadata: map[string]string{"db_path": path},
	})
	require.NoError(t, err)

	alias := filepath.Join(home, "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("SQLite database symlinks unavailable: %v", err)
	}
	t.Setenv("KATA_DB", alias)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = runDaemonWithListen(ctx, "127.0.0.1:0", false)
	require.ErrorContains(t, err, "daemon already running")
}

func TestDaemonStartAcceptsExplicitDevMigrationFlag(t *testing.T) {
	setupKataEnv(t)
	original := runDaemonForeground
	t.Cleanup(func() { runDaemonForeground = original })
	called := false
	runDaemonForeground = func(ctx context.Context, _ string, _ bool) error {
		called = true
		require.True(t, storeopen.DevMigrationAllowed(ctx))
		return nil
	}
	_, _, err := executeRootCapture(t, t.Context(), "daemon", "start", "--foreground", "--allow-dev-migration")
	require.NoError(t, err)
	require.True(t, called)
}

func TestDaemonRefusesDatabaseHeldOutsideHome(t *testing.T) {
	home := setupKataEnv(t)
	path := filepath.Join(home, "kata.db")
	s, err := sqlitestore.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	release, err := daemon.AcquireDatabaseLock(path)
	require.NoError(t, err)
	defer release()
	t.Setenv("KATA_HOME", t.TempDir())
	// Leave KATA_DB pointing to the same file, but use another home and socket.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorContains(t, runDaemonWithListen(ctx, "127.0.0.1:0", false), "daemon already running")
}
