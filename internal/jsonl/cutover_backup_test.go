package jsonl

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrationBackupIncludesUncheckpointedWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	uri := (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
	source, err := sql.Open("sqlite", "file:"+uri)
	require.NoError(t, err)
	defer func() { _ = source.Close() }()
	_, err = source.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE probe(value TEXT); INSERT INTO probe VALUES('committed-in-wal');`)
	require.NoError(t, err)
	backup := path + ".backup"
	require.NoError(t, backupCutoverSource(t.Context(), path, backup))
	copyDB, err := sql.Open("sqlite", backup)
	require.NoError(t, err)
	defer func() { _ = copyDB.Close() }()
	var value string
	require.NoError(t, copyDB.QueryRow(`SELECT value FROM probe`).Scan(&value))
	require.Equal(t, "committed-in-wal", value)
	require.NoError(t, checkCutoverIntegrity(t.Context(), backup))
	// A retry cannot overwrite the verified backup.
	require.ErrorContains(t, backupCutoverSource(t.Context(), path, backup), "create migration backup")
}

func TestMigrationCheckpointRefusesActiveReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	source, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = source.Close() }()
	_, err = source.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE probe(value TEXT); INSERT INTO probe VALUES('before-reader');`)
	require.NoError(t, err)
	reader, err := source.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = reader.Rollback() }()
	var value string
	require.NoError(t, reader.QueryRow(`SELECT value FROM probe`).Scan(&value))
	_, err = source.Exec(`INSERT INTO probe VALUES('after-reader')`)
	require.NoError(t, err)
	before, err := os.Stat(path)
	require.NoError(t, err)
	// A reader pins committed WAL frames: TRUNCATE reports busy and must not
	// remove their journal or replace the source underneath the reader.
	require.ErrorContains(t, checkpointCutoverSource(t.Context(), path), "migration source is busy")
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "busy source must not be replaced")
	var count int
	require.NoError(t, source.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, reader.QueryRow(`SELECT COUNT(*) FROM probe`).Scan(&count))
	require.Equal(t, 1, count, "reader must retain its original snapshot")
	wal, err := os.Stat(path + "-wal")
	require.NoError(t, err)
	require.Positive(t, wal.Size())
}
