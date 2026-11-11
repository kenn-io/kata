package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkFormerInstanceIdentities(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "restore-source")
	require.NoError(t, err)
	_, _, err = source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "restore", Author: "operator"})
	require.NoError(t, err)
	var records []db.ImportRecord
	var events []db.EventExport
	for row, err := range source.ExportMeta(ctx) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportProjects(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportIssues(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		records = append(records, &row)
	}
	for row, err := range source.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		events = append(events, row)
		records = append(records, &row)
	}
	first := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
	aliases, err := first.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{source.InstanceUID()}, aliases)
	var restored []db.EventExport
	for row, err := range first.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		restored = append(restored, row)
	}
	require.Equal(t, events, restored)
	var meta []db.ImportRecord
	for row, err := range first.ExportMeta(ctx) {
		require.NoError(t, err)
		meta = append(meta, &row)
	}
	second := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	require.NoError(t, second.ImportReplay(ctx, meta, db.ImportOptions{NewInstance: true}))
	aliases, err = second.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{source.InstanceUID(), first.InstanceUID()}, aliases)
	ordinary := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, ordinary.Close()) })
	require.NoError(t, ordinary.ImportReplay(ctx, records, db.ImportOptions{}))
	aliases, err = ordinary.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.Empty(t, aliases)
	return nil
}
