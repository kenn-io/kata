package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentEventExportAfterTargetDeletion(t *testing.T, target db.Storage, backend Backend) error {
	t.Helper()
	ctx := t.Context()

	softDeleteSource := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, softDeleteSource.Close()) })
	project, sourceIssue, targetIssue, _, creation := createCommentExportPair(t, softDeleteSource)
	_, _, changed, err := softDeleteSource.SoftDeleteIssue(ctx, targetIssue.ID, "worker")
	require.NoError(t, err)
	require.True(t, changed)

	var liveExport *db.EventExport
	for event, err := range softDeleteSource.ExportEvents(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		if event.UID == creation.UID {
			liveExport = &event
			break
		}
	}
	require.NotNil(t, liveExport, "a live source comment event must survive when only its target issue is soft-deleted")
	require.Nil(t, liveExport.RelatedIssueID, "live-only export must detach the omitted target's numeric FK")
	require.NotNil(t, liveExport.RelatedIssueUID, "live-only export must retain the portable target identity")
	require.Equal(t, targetIssue.UID, *liveExport.RelatedIssueUID)
	require.Equal(t, creation.ContentHash, liveExport.ContentHash)

	liveRecords, err := CollectImportRecords(ctx, softDeleteSource, db.ExportFilter{})
	require.NoError(t, err)
	require.NoError(t, target.ImportReplay(ctx, liveRecords, db.ImportOptions{}))
	liveTargetProject, err := target.ProjectByUID(ctx, project.UID)
	require.NoError(t, err)
	liveTargetEvents, err := target.EventsByUIDs(ctx, liveTargetProject.ID, []string{creation.UID})
	require.NoError(t, err)
	require.Len(t, liveTargetEvents, 1)
	require.Nil(t, liveTargetEvents[0].RelatedIssueID)
	require.NotNil(t, liveTargetEvents[0].RelatedIssueUID)
	require.Equal(t, targetIssue.UID, *liveTargetEvents[0].RelatedIssueUID)
	require.Equal(t, creation.ContentHash, liveTargetEvents[0].ContentHash)
	require.Equal(t, sourceIssue.UID, *liveTargetEvents[0].IssueUID)

	purgedSource := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, purgedSource.Close()) })
	purgedTarget := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, purgedTarget.Close()) })
	purgedProject, _, purgedTargetIssue, targetComment, purgedCreation := createCommentExportPair(t, purgedSource)
	_, err = purgedSource.PurgeIssue(ctx, purgedTargetIssue.ID, "worker", nil)
	require.NoError(t, err)

	includeDeleted := db.ExportFilter{IncludeDeleted: true}
	var purgeExport *db.EventExport
	for event, err := range purgedSource.ExportEvents(ctx, includeDeleted) {
		require.NoError(t, err)
		if event.UID == purgedCreation.UID {
			purgeExport = &event
			break
		}
	}
	require.NotNil(t, purgeExport)
	require.Nil(t, purgeExport.RelatedIssueID, "purge must leave no numeric target FK in the export")
	require.NotNil(t, purgeExport.RelatedIssueUID, "purge export must preserve portable target identity")
	require.Equal(t, purgedTargetIssue.UID, *purgeExport.RelatedIssueUID)
	require.Equal(t, purgedCreation.ContentHash, purgeExport.ContentHash)

	purgedRecords, err := CollectImportRecords(ctx, purgedSource, includeDeleted)
	require.NoError(t, err)
	require.NoError(t, purgedTarget.ImportReplay(ctx, purgedRecords, db.ImportOptions{}))
	purgedTargetProject, err := purgedTarget.ProjectByUID(ctx, purgedProject.UID)
	require.NoError(t, err)
	purgedTargetEvents, err := purgedTarget.EventsByUIDs(ctx, purgedTargetProject.ID, []string{purgedCreation.UID})
	require.NoError(t, err)
	require.Len(t, purgedTargetEvents, 1)
	require.Equal(t, purgedCreation.ContentHash, purgedTargetEvents[0].ContentHash)
	require.NotNil(t, purgedTargetEvents[0].RelatedIssueUID)
	require.Equal(t, purgedTargetIssue.UID, *purgedTargetEvents[0].RelatedIssueUID)
	graph, err := purgedTarget.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: purgedTargetProject.ID})
	require.NoError(t, err)
	require.Equal(t, "removed", graph.Targets[targetComment.UID].Status)
	return nil
}

func createCommentExportPair(t *testing.T, store db.Storage) (db.Project, db.Issue, db.Issue, db.Comment, db.Event) {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Reply source", Author: "worker",
	})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Reply target", Author: "worker",
	})
	require.NoError(t, err)
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: target.ID, Author: "reviewer", Body: "Finding",
	})
	require.NoError(t, err)
	_, creation, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Response",
		ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	return project, source, target, targetComment, creation
}
