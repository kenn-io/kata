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
		records = append(records, &row)
	}
	first := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
	aliases, err := first.FormerInstanceUIDs(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{source.InstanceUID()}, aliases)
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

func checkRestoredRetainedIssueMove(t *testing.T, source db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := source.CreateProject(ctx, "restore-source")
	require.NoError(t, err)
	issue, created, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "local task", Author: "old-actor"})
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: 1, HubProjectUID: project.UID, Actor: "new-actor", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	kept, err := source.KeepFederationEventLocal(ctx, project.ID, created.ID)
	require.NoError(t, err)
	require.True(t, kept)
	var records []db.ImportRecord
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
		records = append(records, &row)
	}
	restored := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, restored.Close()) })
	require.NoError(t, restored.ImportReplay(ctx, records, db.ImportOptions{NewInstance: true}))
	project, err = restored.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	issue, err = restored.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	target, err := restored.CreateProject(ctx, "restore-target")
	require.NoError(t, err)
	bindRejoinSpokes(t, restored, project, target)
	requireRejoinMove(t, restored, issue, project, target, db.ErrFederatedMoveUnsupported)
	return nil
}
