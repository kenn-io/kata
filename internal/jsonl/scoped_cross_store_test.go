package jsonl

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/testenv"
)

func TestScopedBackupCrossStoreExactBytes(t *testing.T) {
	f := dbtest.ScopedOwnerBackupFixture(t)
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	require.NoError(t, source.ImportReplay(ctx, f.Records, db.ImportOptions{}))
	var sqliteBackup bytes.Buffer
	require.NoError(t, Export(ctx, source, &sqliteBackup, ExportOptions{}))
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	postgres, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "scoped_roundtrip", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = postgres.Close() })
	require.NoError(t, Import(ctx, bytes.NewReader(sqliteBackup.Bytes()), postgres))
	var pgBackup bytes.Buffer
	require.NoError(t, Export(ctx, postgres, &pgBackup, ExportOptions{}))
	assertScopedBackupRowsEqual(t, sqliteBackup.Bytes(), pgBackup.Bytes())
	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "restored.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	require.NoError(t, Import(ctx, bytes.NewReader(pgBackup.Bytes()), target))
	var final bytes.Buffer
	require.NoError(t, Export(ctx, target, &final, ExportOptions{}))
	assertScopedBackupRowsEqual(t, sqliteBackup.Bytes(), final.Bytes())
	require.Equal(t, source.InstanceUID(), postgres.InstanceUID())
	require.Equal(t, source.InstanceUID(), target.InstanceUID())
	// A complete graph failure cannot mutate an already populated PostgreSQL
	// target, including its exact source/result/control and verification history.
	envelopes, err := NewDecoder(bytes.NewReader(sqliteBackup.Bytes())).ReadAll(ctx)
	require.NoError(t, err)
	var broken bytes.Buffer
	encoder := NewEncoder(&broken)
	for _, env := range envelopes {
		if env.Kind != KindScopedPrivateSource {
			require.NoError(t, encoder.Write(env))
		}
	}
	require.Error(t, Import(ctx, bytes.NewReader(broken.Bytes()), postgres))
	var unchanged bytes.Buffer
	require.NoError(t, Export(ctx, postgres, &unchanged, ExportOptions{}))
	require.True(t, bytes.Equal(pgBackup.Bytes(), unchanged.Bytes()), "failed PostgreSQL restore changes no target rows")
}

func assertScopedBackupRowsEqual(t *testing.T, want, got []byte) {
	t.Helper()
	scoped := func(raw []byte) []Envelope {
		records, err := NewDecoder(bytes.NewReader(raw)).ReadAll(t.Context())
		require.NoError(t, err)
		rows := []Envelope{}
		for _, env := range records {
			if db.ScopedImportRank(string(env.Kind)) >= 0 || env.Kind == KindRootKey {
				rows = append(rows, env)
			}
		}
		return rows
	}
	expected, actual := scoped(want), scoped(got)
	require.Equal(t, len(expected), len(actual))
	for n, row := range expected {
		require.Equal(t, row.Kind, actual[n].Kind)
		require.True(t, bytes.Equal(row.Data, actual[n].Data), "exact signed/retained owner row changed: %s row %d", row.Kind, n)
	}
}
