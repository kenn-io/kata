package jsonl_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/jsonl"
)

// Contract vmr7: a physically unhealthy source cannot be migrated, even if
// its exported domain rows are readable. Preserve the source for recovery.
func TestAutoCutoverRefusesUnhealthySource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV1DB(t, path)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`CREATE TABLE integrity_probe(value TEXT);
 INSERT INTO integrity_probe VALUES('example');
 CREATE INDEX integrity_probe_index ON integrity_probe(value);
 PRAGMA writable_schema=ON;
 UPDATE sqlite_master SET rootpage=(SELECT rootpage FROM sqlite_master WHERE name='integrity_probe') WHERE name='integrity_probe_index';`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	raw, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	var check string
	require.NoError(t, raw.QueryRow(`PRAGMA integrity_check`).Scan(&check))
	require.NotEqual(t, "ok", check, "fixture must reproduce physical corruption")
	require.NoError(t, raw.Close())
	before, err := os.ReadFile(path) //nolint:gosec // G304: path is a temporary database created by this test.
	require.NoError(t, err)
	err = jsonl.AutoCutover(t.Context(), path)
	require.ErrorContains(t, err, "integrity_check")
	after, err := os.ReadFile(path) //nolint:gosec // G304: path is a temporary database created by this test.
	require.NoError(t, err)
	require.Equal(t, before, after)
	backups, err := filepath.Glob(path + ".bak.*")
	require.NoError(t, err)
	require.Empty(t, backups)
	assertNoCutoverTemps(t, path)
}
