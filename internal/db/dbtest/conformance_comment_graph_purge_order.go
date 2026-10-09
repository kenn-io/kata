package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCommentGraphPurgeEvidenceOrder(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	projectUID, err := uid.New()
	require.NoError(t, err)
	sourceUID, err := uid.New()
	require.NoError(t, err)
	targetUID, err := uid.New()
	require.NoError(t, err)
	targetCommentUID, err := uid.New()
	require.NoError(t, err)
	snapshotReplyUID, err := uid.New()
	require.NoError(t, err)

	const importedAt = "2026-01-01T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{
			ID: 1, UID: projectUID, Name: "spoke-project", CreatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1,
		},
		&db.IssueExport{
			ID: 1, UID: sourceUID, ProjectID: 1, ShortID: strings.ToLower(sourceUID[len(sourceUID)-4:]), Title: "Reply source",
			Status: "open", Author: "worker", CreatedAt: importedAt, UpdatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.IssueExport{
			ID: 2, UID: targetUID, ProjectID: 1, ShortID: strings.ToLower(targetUID[len(targetUID)-4:]), Title: "Purged target",
			Status: "open", Author: "worker", CreatedAt: importedAt, UpdatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.CommentExport{
			ID: 1, UID: targetCommentUID, IssueID: 2, Author: "reviewer", Body: "Finding",
			CreatedAt: importedAt,
		},
		&db.CommentExport{
			ID: 2, UID: snapshotReplyUID, IssueID: 1, Author: "worker", Body: "Imported response",
			CreatedAt: importedAt, ReplyToUID: targetCommentUID, ReplyKind: "reply",
		},
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))
	project, err := store.ProjectByUID(ctx, projectUID)
	require.NoError(t, err)
	source, err := store.IssueByUID(ctx, sourceUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	target, err := store.IssueByUID(ctx, targetUID, db.IncludeDeletedNo)
	require.NoError(t, err)

	eventBackedReply, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Later response",
		ReplyToUID: targetCommentUID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID})
	require.NoError(t, err)
	require.Contains(t, graph.Targets, targetCommentUID)
	require.Equal(t, "removed", graph.Targets[targetCommentUID].Status,
		"purge evidence from any surviving reply must resolve an imported reply row without a create event")
	for _, uid := range []string{snapshotReplyUID, eventBackedReply.UID} {
		found := false
		for _, record := range graph.Comments {
			if record.Comment.UID == uid {
				found = true
				require.Equal(t, "removed", graph.Targets[record.Comment.ReplyToUID].Status)
			}
		}
		require.True(t, found, "surviving reply %s must remain in the graph", uid)
	}

	sourceProject, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	targetProject, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	sourceIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: sourceProject.ID, Title: "Early reply source", Author: "worker",
	})
	require.NoError(t, err)
	targetIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: targetProject.ID, Title: "Late target", Author: "worker",
	})
	require.NoError(t, err)
	targetComment, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: targetIssue.ID, Author: "reviewer", Body: "Finding arrives later",
	})
	require.NoError(t, err)
	earlyReply, earlyEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: sourceIssue.ID, Author: "worker", Body: "Response before target arrives",
		ReplyToUID: targetComment.UID, ReplyKind: "reply",
	})
	require.NoError(t, err)
	require.Nil(t, earlyEvent.RelatedIssueUID, "the early response has no target issue envelope yet")
	currentTarget, err := store.IssueByID(ctx, targetIssue.ID)
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID: targetIssue.ID, FromProjectID: targetProject.ID, ToProjectID: sourceProject.ID,
		IfMatchRev: currentTarget.Revision, Actor: "worker",
	})
	require.NoError(t, err)
	lateReply, lateEvent, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: sourceIssue.ID, Author: "worker", Body: "Response after target arrives",
		ReplyToUID: targetComment.UID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	require.NotNil(t, lateEvent.RelatedIssueUID)
	require.Equal(t, targetIssue.UID, *lateEvent.RelatedIssueUID)
	_, err = store.PurgeIssue(ctx, targetIssue.ID, "worker", nil)
	require.NoError(t, err)

	arrivedGraph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: sourceProject.ID})
	require.NoError(t, err)
	require.Equal(t, "removed", arrivedGraph.Targets[targetComment.UID].Status,
		"the later response must upgrade purge evidence cached from an earlier reply")
	for _, uid := range []string{earlyReply.UID, lateReply.UID} {
		found := false
		for _, record := range arrivedGraph.Comments {
			if record.Comment.UID == uid {
				found = true
				require.Equal(t, "removed", arrivedGraph.Targets[record.Comment.ReplyToUID].Status)
			}
		}
		require.True(t, found, "surviving reply %s must remain in the graph", uid)
	}
	return nil
}
