package jsonl

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func exportedMetaValues(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	envelopes, err := NewDecoder(bytes.NewReader(raw)).ReadAll(t.Context())
	require.NoError(t, err)
	values := make(map[string]string)
	for _, envelope := range envelopes {
		if envelope.Kind != KindMeta {
			continue
		}
		var record metaRecord
		require.NoError(t, json.Unmarshal(envelope.Data, &record))
		values[record.Key] = record.Value
	}
	return values
}

func TestFederationOperationMarkersStayInSameInstanceBackups(t *testing.T) {
	ctx := t.Context()
	source, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	project, err := source.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	markers := map[string]string{
		"federation.publication.v1/" + project.UID: `{"operation_uid":"01K00000000000000000000005","status":"published"}`,
		"federation.adoption.v1/" + project.UID:    `{"operation_uid":"01K00000000000000000000005","phase":"transmitting"}`,
	}
	const ordinaryMetaKey = "ordinary-example"
	const ordinaryMetaValue = "ordinary-value"
	for key, value := range markers {
		_, err := source.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)`, key, value)
		require.NoError(t, err)
	}
	_, err = source.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)`, ordinaryMetaKey, ordinaryMetaValue)
	require.NoError(t, err)

	var backup bytes.Buffer
	require.NoError(t, Export(ctx, source, &backup, ExportOptions{IncludeDeleted: true}))
	var cutover bytes.Buffer
	require.NoError(t, exportForCutover(ctx, source, &cutover, ExportOptions{IncludeDeleted: true}))
	var portable bytes.Buffer
	require.NoError(t, Export(ctx, source, &portable, ExportOptions{ProjectID: project.ID, IncludeDeleted: true}))
	backupMeta := exportedMetaValues(t, backup.Bytes())

	for key, value := range markers {
		require.Equal(t, value, exportedMetaValues(t, backup.Bytes())[key], "same-instance backup keeps operation state")
		require.Equal(t, value, exportedMetaValues(t, cutover.Bytes())[key], "SQLite cutover keeps operation state")
		_, present := exportedMetaValues(t, portable.Bytes())[key]
		require.False(t, present, "independent project copies must not inherit relay operations")
	}
	require.Equal(t, ordinaryMetaValue, backupMeta[ordinaryMetaKey], "full backup retains ordinary metadata alongside relay operations")

	target, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NoError(t, Import(ctx, bytes.NewReader(backup.Bytes()), target))
	restoredMeta := exportedMetaValues(t, mustExportAll(ctx, t, target))
	require.Equal(t, ordinaryMetaValue, restoredMeta[ordinaryMetaKey], "restore retains ordinary metadata")
	for key, value := range markers {
		require.Equal(t, value, restoredMeta[key], "restore keeps exact operation identity")
	}
}

func mustExportAll(ctx context.Context, t *testing.T, store *sqlitestore.Store) []byte {
	t.Helper()
	var raw bytes.Buffer
	require.NoError(t, Export(ctx, store, &raw, ExportOptions{IncludeDeleted: true}))
	return raw.Bytes()
}
