package jsonl

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestImportCutoverTargetRejectsMissingExportEvent(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "kata.db")
	source, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = source.Close() })
	project, err := source.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "History to preserve", Author: "tester",
	})
	require.NoError(t, err)
	require.NoError(t, source.Close())

	exportPath := path + ".jsonl"
	expectedEvents, err := exportCutoverSource(ctx, path, exportPath)
	require.NoError(t, err)
	contents, err := os.ReadFile(exportPath) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	records, err := NewDecoder(bytes.NewReader(contents)).ReadAll(ctx)
	require.NoError(t, err)
	var incomplete bytes.Buffer
	encoder := NewEncoder(&incomplete)
	var dropped bool
	for _, record := range records {
		if record.Kind == KindEvent && !dropped {
			dropped = true
			continue
		}
		require.NoError(t, encoder.Write(record))
	}
	require.True(t, dropped)
	require.NoError(t, os.WriteFile(exportPath, incomplete.Bytes(), 0o600))

	err = importCutoverTarget(ctx, exportPath, path+".target", expectedEvents)

	require.ErrorContains(t, err, "event count mismatch")
}
