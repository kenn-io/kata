package storeopen_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
)

func TestSQLiteConfiguredBusyTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[storage.sqlite]\nbusy_timeout = \"1ms\"\n"), 0600))
	ctx := context.Background()
	path := filepath.Join(home, "test.db")
	first, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Close()) }()
	raw := first.(*sqlitestore.Store)
	// A second connection checks the DSN pragma, not only the first connection.
	for range 2 {
		conn, err := raw.Conn(ctx)
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		var timeout int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout))
		require.Equal(t, 1, timeout)
	}
	// Test-local DDL keeps this independent from product schema objects.
	_, err = raw.ExecContext(ctx, "CREATE TABLE contention_test (value INTEGER)")
	require.NoError(t, err)
	lock, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1)")
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()
	hold, err := lock.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, hold.Close()) }()
	_, err = hold.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	defer func() { _, _ = hold.ExecContext(ctx, "ROLLBACK") }()
	started := time.Now()
	_, err = raw.ExecContext(ctx, "INSERT INTO contention_test VALUES (1)")
	require.True(t, sqlitestore.IsTransient(err), "short configured timeout should report busy: %v", err)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	// Open before taking the second lock: startup itself writes.
	_, err = hold.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, err)
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "1s")
	second, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()
	long := second.(*sqlitestore.Store)
	var timeout int
	require.NoError(t, long.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout))
	require.Equal(t, 1000, timeout)
	_, err = hold.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		//nolint:kennlint // sleeptest: SQLite busy polling needs real elapsed time while another handle owns the lock.
		time.Sleep(50 * time.Millisecond)
		_, err := hold.ExecContext(ctx, "COMMIT")
		done <- err
	}()
	_, err = long.ExecContext(ctx, "INSERT INTO contention_test VALUES (2)")
	require.NoError(t, err, "longer configured timeout should survive the same contention")
	require.NoError(t, <-done)
}

func TestSQLiteBusyTimeoutInvalidBeforeOpen(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "0s")
	path := filepath.Join(t.TempDir(), "missing.db")
	_, err := storeopen.Open(context.Background(), path, db.Serving())
	require.ErrorContains(t, err, "storage.sqlite.busy_timeout")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "validation must happen before creating storage")
}

func TestSQLiteBusyTimeoutExplicitOptionWins(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "")
	store, err := storeopen.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), db.WithSQLiteBusyTimeout(2*time.Second))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	var timeout int
	require.NoError(t, store.(*sqlitestore.Store).QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&timeout))
	require.Equal(t, 2000, timeout)
}

func TestSQLiteReadOnlyOpenUsesConfiguredBusyTimeout(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_SQLITE_BUSY_TIMEOUT", "")
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	writable, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, writable.Close())

	readonly, err := storeopen.OpenWithConfig(ctx, path, storeopen.Config{SQLiteBusyTimeout: 2 * time.Second}, db.ReadOnly())
	require.NoError(t, err)
	defer func() { require.NoError(t, readonly.Close()) }()
	sqliteStore, ok := readonly.(*sqlitestore.Store)
	require.True(t, ok)

	var timeout int
	require.NoError(t, sqliteStore.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout))
	require.Equal(t, 2000, timeout)
}
