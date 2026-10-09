package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkCommentGraphPurgeEvidenceFromHiddenSource(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	projectUID, err := uid.New()
	require.NoError(t, err)
	visibleSourceUID, err := uid.New()
	require.NoError(t, err)
	evidenceSourceUID, err := uid.New()
	require.NoError(t, err)
	targetUID, err := uid.New()
	require.NoError(t, err)
	targetCommentUID, err := uid.New()
	require.NoError(t, err)
	snapshotReplyUID, err := uid.New()
	require.NoError(t, err)

	const createdAt = "2026-01-01T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{ID: 1, UID: projectUID, Name: "spoke-project", CreatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1},
		&db.IssueExport{ID: 1, UID: visibleSourceUID, ProjectID: 1, ShortID: strings.ToLower(visibleSourceUID[len(visibleSourceUID)-4:]), Title: "Visible reply source", Status: "open", Author: "worker", CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&db.IssueExport{ID: 2, UID: evidenceSourceUID, ProjectID: 1, ShortID: strings.ToLower(evidenceSourceUID[len(evidenceSourceUID)-4:]), Title: "Evidence source", Status: "open", Author: "worker", CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&db.IssueExport{ID: 3, UID: targetUID, ProjectID: 1, ShortID: strings.ToLower(targetUID[len(targetUID)-4:]), Title: "Purged target", Status: "open", Author: "worker", CreatedAt: createdAt, UpdatedAt: createdAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1},
		&db.CommentExport{ID: 1, UID: targetCommentUID, IssueID: 3, Author: "reviewer", Body: "Finding", CreatedAt: createdAt},
		&db.CommentExport{ID: 2, UID: snapshotReplyUID, IssueID: 1, Author: "worker", Body: "Imported response", CreatedAt: createdAt, ReplyToUID: targetCommentUID, ReplyKind: "reply"},
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))
	project, err := store.ProjectByUID(ctx, projectUID)
	require.NoError(t, err)
	visibleSource, err := store.IssueByUID(ctx, visibleSourceUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	evidenceSource, err := store.IssueByUID(ctx, evidenceSourceUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	target, err := store.IssueByUID(ctx, targetUID, db.IncludeDeletedNo)
	require.NoError(t, err)

	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: evidenceSource.ID, Author: "worker", Body: "Evidence for the target purge",
		ReplyToUID: targetCommentUID, ReplyKind: "confirm",
	})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)
	_, _, changed, err := store.SoftDeleteIssue(ctx, evidenceSource.ID, "worker")
	require.NoError(t, err)
	require.True(t, changed)

	graph, err := store.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID})
	require.NoError(t, err)
	require.Contains(t, graph.Targets, targetCommentUID)
	require.Equal(t, "removed", graph.Targets[targetCommentUID].Status,
		"retained purge evidence from a hidden source reply must resolve another visible reply to the same target")
	require.Contains(t, []string{snapshotReplyUID}, graph.Comments[0].Comment.UID)
	require.Equal(t, visibleSource.ID, graph.Comments[0].Comment.IssueID)
	return nil
}
