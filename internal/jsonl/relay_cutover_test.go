package jsonl_test

import (
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

//go:embed testdata/schema_v32.sql
var relaySchema32 string

func TestRelayCutoverUpgradesFrozenVersion32(t *testing.T) {
	require.Greater(t, db.CurrentSchemaVersion(), 32, "the next real cutover must preserve signed state")
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	seed, pin, receipt := dbtest.FrozenRelay32Fixture(t)
	_, err = source.ExecContext(ctx, relaySchema32+seed)
	require.NoError(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	dbtest.AssertFrozenRelay32(t, upgraded, pin, receipt)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
}
