package testenv_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver as "pgx"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// TestNewPostgresContainerReturnsUsableDSN proves the helper exposes a DSN
// that the pgx stdlib driver can open. -short skips.
func TestNewPostgresContainerReturnsUsableDSN(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	var one int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT 1").Scan(&one))
	require.Equal(t, 1, one)
}

func TestExplicitPostgresServiceCreatesIsolatedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ctx := context.Background()
	baseDSN, baseCleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(baseCleanup)
	t.Setenv("KATA_TEST_POSTGRES_DSN", baseDSN)

	isolatedDSN, isolatedCleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(isolatedCleanup)
	require.NotEqual(t, baseDSN, isolatedDSN)

	db, err := sql.Open("pgx", isolatedDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var databaseName string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName))
	require.Contains(t, databaseName, "kata_test_")
}

// Starting a PostgreSQL container dominates small storage tests, so every
// test in one process shares a server and receives its own database.
func TestAutomaticPostgresContainersShareOneServerWithIsolatedDatabases(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	t.Setenv("KATA_TEST_POSTGRES_DSN", "")
	ctx := context.Background()
	open := func(t *testing.T) (*sql.DB, func()) {
		t.Helper()
		dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
		database, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		return database, func() {
			require.NoError(t, database.Close())
			cleanup()
		}
	}
	first, closeFirst := open(t)
	second, closeSecond := open(t)
	t.Cleanup(closeSecond)
	var firstStart, secondStart, firstName, secondName string
	require.NoError(t, first.QueryRowContext(ctx, `SELECT pg_postmaster_start_time()::text, current_database()`).Scan(&firstStart, &firstName))
	require.NoError(t, second.QueryRowContext(ctx, `SELECT pg_postmaster_start_time()::text, current_database()`).Scan(&secondStart, &secondName))
	require.Equal(t, firstStart, secondStart, "tests in one process must reuse the server")
	require.NotEqual(t, firstName, secondName)
	_, err := first.ExecContext(ctx, `CREATE TABLE only_in_first (id int)`)
	require.NoError(t, err)
	var visible bool
	require.NoError(t, second.QueryRowContext(ctx, `SELECT to_regclass('only_in_first') IS NOT NULL`).Scan(&visible))
	require.False(t, visible, "each test must receive an isolated database")
	closeFirst()
	var remaining bool
	require.NoError(t, second.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, firstName).Scan(&remaining))
	require.False(t, remaining, "cleanup must drop the test database")
}
