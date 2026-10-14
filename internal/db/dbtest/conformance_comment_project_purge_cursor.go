package dbtest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkEventlessArchivedProjectPurgeAdvancesSnapshotCursor(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()

	archivedProjectUID, err := uid.New()
	require.NoError(t, err)
	survivingProjectUID, err := uid.New()
	require.NoError(t, err)
	archivedIssueUID, err := uid.New()
	require.NoError(t, err)
	survivingIssueUID, err := uid.New()
	require.NoError(t, err)
	targetCommentUID, err := uid.New()
	require.NoError(t, err)
	replyCommentUID, err := uid.New()
	require.NoError(t, err)
	const importedAt = "2026-01-01T00:00:00.000Z"
	archivedAt := "2026-01-02T00:00:00.000Z"
	records := []db.ImportRecord{
		&db.ProjectExport{
			ID: 1, UID: archivedProjectUID, Name: "archived-project", CreatedAt: importedAt,
			DeletedAt: &archivedAt, Metadata: []byte(`{}`), Revision: 1,
		},
		&db.ProjectExport{
			ID: 2, UID: survivingProjectUID, Name: "spoke-project", CreatedAt: importedAt,
			Metadata: []byte(`{}`), Revision: 1,
		},
		&db.IssueExport{
			ID: 1, UID: archivedIssueUID, ProjectID: 1,
			ShortID: strings.ToLower(archivedIssueUID[len(archivedIssueUID)-4:]),
			Title:   "Archived target", Status: "open", Author: "worker", CreatedAt: importedAt,
			UpdatedAt: importedAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.IssueExport{
			ID: 2, UID: survivingIssueUID, ProjectID: 2,
			ShortID: strings.ToLower(survivingIssueUID[len(survivingIssueUID)-4:]),
			Title:   "Surviving reply source", Status: "open", Author: "worker", CreatedAt: importedAt,
			UpdatedAt: importedAt, Metadata: []byte(`{}`), Revision: 1, ContentRevision: 1,
		},
		&db.CommentExport{
			ID: 1, UID: targetCommentUID, IssueID: 1, Author: "reviewer", Body: "Finding",
			CreatedAt: importedAt,
		},
		&db.CommentExport{
			ID: 2, UID: replyCommentUID, IssueID: 2, Author: "worker", Body: "Imported response",
			CreatedAt: importedAt, ReplyToUID: targetCommentUID, ReplyKind: "reply",
		},
	}
	require.NoError(t, store.ImportReplay(ctx, records, db.ImportOptions{}))

	archivedProject, err := store.ProjectByUID(ctx, archivedProjectUID)
	require.NoError(t, err)

	uiStore, ok := store.(db.UIStore)
	require.True(t, ok, "storage backend must implement db.UIStore")
	before, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		ProjectUID: survivingProjectUID, SelectedIssueUID: survivingIssueUID,
	})
	require.NoError(t, err)
	require.Equal(t, "hidden", before.CommentGraph.Targets[targetCommentUID].Status,
		"the archived target project hides the imported cross-project reply endpoint")

	log, err := store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: archivedProject.ID, Actor: "worker"})
	require.NoError(t, err)
	require.Zero(t, log.EventCount, "the imported archived project has no stored events")
	require.Nil(t, log.EventsDeletedMinID)
	require.Nil(t, log.EventsDeletedMaxID)

	conditional, err := uiStore.ReadUISnapshot(ctx, db.UISnapshotQuery{
		ProjectUID: survivingProjectUID, SelectedIssueUID: survivingIssueUID,
		ReuseAuthorityCursor: &before.Cursor,
	})
	require.NoError(t, err)
	assert.False(t, conditional.AuthorityReused,
		"purging a project must invalidate conditional snapshots even when that project has no events")
	assert.NotNil(t, log.PurgeResetAfterEventID,
		"every successful project purge must reserve a reset cursor")
	assert.Greater(t, conditional.Cursor, before.Cursor,
		"the reset cursor must advance beyond the prior snapshot authority")
	assert.NotEqual(t, "hidden", conditional.CommentGraph.Targets[targetCommentUID].Status,
		"after purge the surviving reply target is unavailable rather than hidden")

	return nil
}
