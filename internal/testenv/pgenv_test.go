package testenv_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // register pgx driver as "pgx"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

// TestNewPostgresContainerReturnsUsableDSN proves the helper starts a
// postgres:17-alpine container, exposes a DSN that the pgx stdlib driver
// can open, and tears the container down when the test ends. -short skips.
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
	require.Contains(t, databaseName, "kata_ci_")
}
