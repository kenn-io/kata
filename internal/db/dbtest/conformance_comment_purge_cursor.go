package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkReplyOnlyPurgeAdvancesSnapshotCursor(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()

	projectUID, err := uid.New()
	require.NoError(t, err)
	targetIssueUID, err := uid.New()
	require.NoError(t, err)
	targetCommentUID, err := uid.New()
	require.NoError(t, err)
	const importedAt = "2026-01-01T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{
			ID: 1, UID: projectUID, Name: "spoke-project", CreatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1,
		},
		&db.IssueExport{
			ID: 1, UID: targetIssueUID, ProjectID: 1,
			ShortID: strings.ToLower(targetIssueUID[len(targetIssueUID)-4:]), Title: "Imported target",
			Status: "open", Author: "worker", CreatedAt: importedAt, UpdatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.CommentExport{
			ID: 1, UID: targetCommentUID, IssueID: 1, Author: "reviewer", Body: "Imported comment",
			CreatedAt: importedAt,
		},
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))

	project, err := store.ProjectByUID(ctx, projectUID)
	require.NoError(t, err)
	target, err := store.IssueByUID(ctx, targetIssueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Reply source", Author: "worker",
	})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: source.ID, Author: "worker", Body: "Reply to imported comment",
		ReplyToUID: targetCommentUID, ReplyKind: "reply",
	})
	require.NoError(t, err)

	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	before, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.Contains(t, uiIssueUIDs(before.Issues), targetIssueUID)

	log, err := store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)
	require.Zero(t, log.EventCount, "the imported target has no events of its own")
	require.Nil(t, log.EventsDeletedMinID, "the surviving reply event is not deleted")
	require.Nil(t, log.EventsDeletedMaxID, "the surviving reply event is not deleted")

	afterCursor, err := uiStore.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Greater(t, afterCursor, before.Cursor,
		"detaching a surviving reply target must invalidate snapshots even when purge deletes no events")

	after, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", ReuseAuthorityCursor: &before.Cursor,
	})
	require.NoError(t, err)
	require.False(t, after.AuthorityReused, "purge must force the browser to rebuild stale collection authority")
	require.Greater(t, after.Cursor, before.Cursor)
	require.NotContains(t, uiIssueUIDs(after.Issues), targetIssueUID)
	return nil
}

func checkImportedCommentPurgeAdvancesSnapshotCursor(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()

	projectUID, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	const importedAt = "2026-01-01T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{
			ID: 1, UID: projectUID, Name: "spoke-project", CreatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1,
		},
		&db.IssueExport{
			ID: 1, UID: issueUID, ProjectID: 1, ShortID: strings.ToLower(issueUID[len(issueUID)-4:]),
			Title: "Imported issue", Status: "open", Author: "worker", CreatedAt: importedAt,
			UpdatedAt: importedAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.CommentExport{
			ID: 1, UID: commentUID, IssueID: 1, Author: "reviewer", Body: "Imported comment",
			CreatedAt: importedAt,
		},
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{MergeProject: true}))

	project, err := store.ProjectByUID(ctx, projectUID)
	require.NoError(t, err)
	issue, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	require.NoError(t, err)
	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	before, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open"})
	require.NoError(t, err)
	require.Contains(t, uiIssueUIDs(before.Issues), issueUID)

	log, err := store.PurgeIssue(ctx, issue.ID, "worker", nil)
	require.NoError(t, err)
	require.Equal(t, project.ID, log.ProjectID)
	require.Equal(t, int64(1), log.CommentCount)
	require.Zero(t, log.EventCount, "the imported issue has no events")
	require.Nil(t, log.EventsDeletedMinID)
	require.Nil(t, log.EventsDeletedMaxID)

	afterCursor, err := uiStore.UIEventCursor(ctx)
	require.NoError(t, err)
	require.Greater(t, afterCursor, before.Cursor,
		"purging imported comments must invalidate snapshots even when no events or replies exist")
	after, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		View: "all-open", ReuseAuthorityCursor: &before.Cursor,
	})
	require.NoError(t, err)
	require.False(t, after.AuthorityReused)
	require.Greater(t, after.Cursor, before.Cursor)
	require.NotContains(t, uiIssueUIDs(after.Issues), issueUID)
	return nil
}
