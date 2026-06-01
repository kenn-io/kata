package jsonl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// openMigratedTestDB opens a SQLite store at path and brings it to current via
// Migrate. Used by cutover and roundtrip tests where the legacy bootstrap-on-
// Open behavior is what they relied on; with Phase 2's runner this becomes
// Open+Migrate. The KATA_HOME env is per-test scoped so snapshot files land
// under t.TempDir().
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func openMigratedTestDB(t *testing.T, ctx context.Context, path string) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}
