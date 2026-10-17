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

// Installing a schema per test dominates small PostgreSQL tests, so a
// template prepares it once per server and each test receives a clone.
func TestPostgresTemplateClonesPreparedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ctx := context.Background()
	prepared := 0
	template := testenv.NewPostgresTemplate(func(ctx context.Context, dsn string) error {
		prepared++
		database, err := sql.Open("pgx", dsn)
		if err != nil {
			return err
		}
		defer func() { _ = database.Close() }()
		_, err = database.ExecContext(ctx, `CREATE TABLE seeded (value text); INSERT INTO seeded VALUES ('from-template')`)
		return err
	})
	open := func(t *testing.T) (*sql.DB, string, func()) {
		t.Helper()
		dsn, cleanup := template.NewDatabase(t, ctx)
		database, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		var name string
		require.NoError(t, database.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name))
		return database, name, func() {
			require.NoError(t, database.Close())
			cleanup()
		}
	}
	first, firstName, closeFirst := open(t)
	second, _, closeSecond := open(t)
	t.Cleanup(closeSecond)
	require.Equal(t, 1, prepared, "the template is prepared once per server")
	for _, database := range []*sql.DB{first, second} {
		var value string
		require.NoError(t, database.QueryRowContext(ctx, `SELECT value FROM seeded`).Scan(&value))
		require.Equal(t, "from-template", value)
	}
	_, err := first.ExecContext(ctx, `INSERT INTO seeded VALUES ('only-in-first')`)
	require.NoError(t, err)
	var copies int
	require.NoError(t, second.QueryRowContext(ctx, `SELECT count(*) FROM seeded`).Scan(&copies))
	require.Equal(t, 1, copies, "each clone must be isolated")
	closeFirst()
	var remaining bool
	require.NoError(t, second.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, firstName).Scan(&remaining))
	require.False(t, remaining, "cleanup must drop the clone")
}

// Templates outlive the test process that built them. A later process drops
// templates whose owner is gone so a long-lived server does not fill up.
func TestPostgresTemplateDropsAbandonedTemplates(t *testing.T) {
	if testing.Short() {
		t.Skip("testcontainer requires docker; skip on -short")
	}
	ctx := context.Background()
	serverDSN, serverCleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(serverCleanup)
	t.Setenv("KATA_TEST_POSTGRES_DSN", serverDSN)
	server, err := sql.Open("pgx", serverDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	_, err = server.ExecContext(ctx, `CREATE DATABASE kata_template_abandoned`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = server.ExecContext(context.Background(), `DROP DATABASE IF EXISTS kata_template_abandoned`)
	})
	exists := func(name string) bool {
		var found bool
		require.NoError(t, server.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&found))
		return found
	}
	noop := func(context.Context, string) error { return nil }

	firstDSN, firstCleanup := testenv.NewPostgresTemplate(noop).NewDatabase(t, ctx)
	t.Cleanup(firstCleanup)
	require.False(t, exists("kata_template_abandoned"), "a template without a live owner must be dropped")
	first, err := sql.Open("pgx", firstDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	var live []string
	rows, err := server.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'kata\_template\_%'`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		live = append(live, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, live)

	_, secondCleanup := testenv.NewPostgresTemplate(noop).NewDatabase(t, ctx)
	t.Cleanup(secondCleanup)
	for _, name := range live {
		require.True(t, exists(name), "a template whose owner is still running must survive: %s", name)
	}
}
