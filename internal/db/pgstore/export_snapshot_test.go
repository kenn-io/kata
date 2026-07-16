package pgstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestExportSnapshotAndPortableIdentityHighWater(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "export_snapshot", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "snapshot-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "snapshot issue", Author: "worker",
	})
	require.NoError(t, err)

	snapshotStorage, release, err := store.BeginExportSnapshot(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = release() })
	projects := collectPGExport(t, snapshotStorage.ExportProjects(ctx, db.ExportFilter{}))
	require.NotEmpty(t, projects)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "worker", Force: true,
	})
	require.NoError(t, err)
	_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: project.ID, Actor: "worker"})
	require.NoError(t, err)
	issues := collectPGExport(t, snapshotStorage.ExportIssues(ctx, db.ExportFilter{}))
	require.Len(t, issues, 1)
	assert.Equal(t, issue.UID, issues[0].UID)

	sequences := collectPGExport(t, store.ExportSequences(ctx))
	sequenceByName := make(map[string]int64, len(sequences))
	for _, sequence := range sequences {
		sequenceByName[sequence.Name] = sequence.Seq
	}
	assert.GreaterOrEqual(t, sequenceByName["projects"], project.ID)
	purgeRows := collectPGExport(t, store.ExportProjectPurgeLog(ctx, db.ExportFilter{}))
	require.Len(t, purgeRows, 1)
	require.NotNil(t, purgeRows[0].PurgeResetAfterEventID)
	assert.GreaterOrEqual(t, sequenceByName["events"], *purgeRows[0].PurgeResetAfterEventID)
}

func collectPGExport[T any](t *testing.T, sequence func(func(T, error) bool)) []T {
	t.Helper()
	var records []T
	for record, err := range sequence {
		require.NoError(t, err)
		records = append(records, record)
	}
	return records
}
