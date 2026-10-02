package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitelock"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	kitdaemon "go.kenn.io/kit/daemon"
)

func TestRuntimeGuardSkipsUnusableNamespacesWithWarning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permissions")
	}
	for _, current := range []bool{false, true} {
		t.Run(map[bool]string{false: "other", true: "current"}[current], func(t *testing.T) {
			home := setupKataEnv(t)
			path := filepath.Join(home, "kata.db")
			require.NoError(t, os.WriteFile(path, nil, 0600))
			ns, err := daemon.NewNamespace()
			require.NoError(t, err)
			require.NoError(t, ns.EnsureDirs())
			badDir := filepath.Join(filepath.Dir(ns.DataDir), "stray")
			if current {
				badDir = ns.DataDir
			}
			require.NoError(t, os.MkdirAll(badDir, 0700))
			require.NoError(t, os.Chmod(badDir, 0755)) //nolint:gosec // G302: deliberately unusable runtime directory.
			stderr := captureProcessStderr(t, func() {
				_, found, err := liveSQLiteDaemonRecord(ns.DataDir, path)
				require.NoError(t, err)
				require.False(t, found)
			})
			require.Contains(t, stderr, "warning:")
			require.Contains(t, stderr, badDir)
		})
	}
}

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
	lock, err := sqlitelock.Acquire(path)
	require.NoError(t, err)
	defer lock.Release()
	t.Setenv("KATA_HOME", t.TempDir())
	// Leave KATA_DB pointing to the same file, but use another home and socket.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.ErrorContains(t, runDaemonWithListen(ctx, "127.0.0.1:0", false), "daemon already running")
}
