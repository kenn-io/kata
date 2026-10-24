package jsonl

import (
	"bytes"
	"database/sql"
	_ "embed"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

//go:embed testdata/schema_v34.sql
var scopedSQLiteSchema34 string

func TestScopedSchemaSQLite34Cutover(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	native, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = native.ExecContext(ctx, scopedSQLiteSchema34+`
 INSERT INTO meta(key,value) VALUES('schema_version','34'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','example-project');`)
	require.NoError(t, err)
	require.NoError(t, native.Close())
	// Pre-existing interrupted work must never be overwritten by a second swap.
	interrupted := path + ".import.tmp.db"
	require.NoError(t, os.WriteFile(interrupted, []byte("interrupted evidence"), 0600))
	require.ErrorIs(t, AutoCutover(ctx, path), ErrCutoverInProgress)
	version, err := sqlitestore.PeekSchemaVersion(ctx, path)
	require.NoError(t, err)
	require.Equal(t, 34, version)
	require.NoError(t, os.Remove(interrupted))
	require.NoError(t, AutoCutover(ctx, path))
	current, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = current.Close() })
	version, err = current.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 35, version)
	var count int
	require.NoError(t, current.QueryRowContext(ctx, `SELECT count(*) FROM federation_scoped_views`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, current.QueryRowContext(ctx, `SELECT count(*) FROM federation_canonical_close_state`).Scan(&count))
	require.Zero(t, count, "unknown legacy close proof remains absent")
	backups, err := filepath.Glob(path + ".bak.*")
	require.NoError(t, err)
	require.Len(t, backups, 1)
	version, err = sqlitestore.PeekSchemaVersion(ctx, backups[0])
	require.NoError(t, err)
	require.Equal(t, 34, version)
	// A read-only old exporter must not touch nonexistent v2 tables.
	old, err := sqlitestore.Open(ctx, backups[0], db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = old.Close() })
	var backup bytes.Buffer
	require.NoError(t, Export(ctx, old, &backup, ExportOptions{}))
	require.NotContains(t, backup.String(), `"kind":"federation_scoped_view"`)
}
