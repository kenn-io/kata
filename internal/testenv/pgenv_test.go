package testenv_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver as "pgx"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

func TestExplicitPostgresServiceUsesConfiguredServer(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL service test skipped on -short")
	}
	baseDSN := os.Getenv("KATA_TEST_POSTGRES_DSN")
	if baseDSN == "" {
		t.Skip("explicit PostgreSQL service requires KATA_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	base, err := sql.Open("pgx", baseDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	var baseStart string
	require.NoError(t, base.QueryRowContext(ctx, `SELECT pg_postmaster_start_time()::text`).Scan(&baseStart))

	isolatedDSN, isolatedCleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(isolatedCleanup)

	db, err := sql.Open("pgx", isolatedDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	var isolatedStart string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_postmaster_start_time()::text`).Scan(&isolatedStart))
	require.Equal(t, baseStart, isolatedStart, "the isolated database must use the explicit server")
}

// Template clones share the configured or automatic PostgreSQL server,
// prepare their seed data once, and isolate each test's database.
func TestPostgresTemplateSharesServerWithIsolatedDatabases(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL template test skipped on -short")
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
	open := func(t *testing.T) (*sql.DB, func()) {
		t.Helper()
		dsn, cleanup := template.NewDatabase(t, ctx)
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
	require.False(t, remaining, "cleanup must drop the test database")
}

// Templates outlive the test process that built them. A later process drops
// templates whose owner is gone so a long-lived server does not fill up.
func TestPostgresTemplateDropsAbandonedTemplates(t *testing.T) {
	if testing.Short() {
		t.Skip("PostgreSQL template test skipped on -short")
	}
	ctx := context.Background()
	serverDSN, serverCleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(serverCleanup)
	t.Setenv("KATA_TEST_POSTGRES_DSN", serverDSN)
	server, err := sql.Open("pgx", serverDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	abandoned := fmt.Sprintf("kata_template_%d_%d", os.Getpid(), ^uint64(0))
	_, err = server.ExecContext(ctx, `CREATE DATABASE "`+abandoned+`"`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = server.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+abandoned+`"`)
	})
	exists := func(name string) bool {
		var found bool
		require.NoError(t, server.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&found))
		return found
	}
	noop := func(context.Context, string) error { return nil }

	_, firstCleanup := testenv.NewPostgresTemplate(noop).NewDatabase(t, ctx)
	t.Cleanup(firstCleanup)
	require.False(t, exists(abandoned), "a template without a live owner must be dropped")
	var live []string
	rows, err := server.QueryContext(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1`, fmt.Sprintf(`kata\_template\_%d\_%%`, os.Getpid()))
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
