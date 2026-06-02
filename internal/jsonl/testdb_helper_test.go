package jsonl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// openCutoverTargetDB opens a SQLite store at path. sqlitestore.Open
// bootstraps the canonical schema in one transaction when the file is
// fresh, so the returned handle is immediately ready for use. KATA_HOME
// is scoped to a per-test TempDir so snapshot files land under it.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func openCutoverTargetDB(t *testing.T, ctx context.Context, path string) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}
