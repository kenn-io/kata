package sqlitestore

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestExternalCommentsUseCanonicalSQLiteTimestampOrdering(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Timestamp ordering", Author: "tester",
	})
	require.NoError(t, err)
	binding, _, err := store.CreateExternalRootBinding(ctx, db.CreateExternalRootBindingParams{
		ProjectID: project.ID, IssueID: issue.ID,
		ConnectorInstance: "notes", ExternalRootKey: "root-ordering",
		ExternalAccountKey: "opaque-account", Actor: "tester",
		ReceiveCommentsAfter: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	claimedAt := time.Date(2026, 8, 20, 10, 1, 0, 0, time.UTC)
	claimed, acquired, err := store.ClaimExternalRootBinding(
		ctx, binding.ID, "claim-ordering", claimedAt, claimedAt.Add(-time.Minute),
	)
	require.NoError(t, err)
	require.True(t, acquired)

	local, _, err := store.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "Local middle",
	})
	require.NoError(t, err)
	middle := local.CreatedAt.UTC()
	var storedLocal string
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT CAST(created_at AS TEXT) FROM comments WHERE id=?`, local.ID,
	).Scan(&storedLocal))
	assert.Equal(t, middle.Format(sqliteCommentTimeFormat), storedLocal)

	middle = middle.Truncate(time.Millisecond)
	_, err = store.ExecContext(ctx, `UPDATE comments SET created_at=? WHERE id=?`,
		middle.Format(sqliteTimeFormat), local.ID)
	require.NoError(t, err)

	earlier := middle.Add(-100 * time.Microsecond)
	_, _, created, err := store.UpsertExternalCommentProjection(ctx, db.ExternalCommentProjectionParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken,
		ExternalID: "provider-comment", ExternalRevision: "comment-revision",
		Body: "External earlier", ExternalActorID: "actor-one", ExternalActorName: "Contributor",
		ExternalCreatedAt: earlier, ExternalUpdatedAt: earlier,
		IntegrationActor: "connector:notes",
	})
	require.NoError(t, err)
	require.True(t, created)

	later := middle.Add(100 * time.Microsecond)
	_, _, created, err = store.EnsureExternalRootLifecycleRequest(ctx, db.ExternalCommentProjectionParams{
		BindingID: claimed.ID, ClaimToken: claimed.ClaimToken,
		ExternalID: "lifecycle:complete:revision-one", ExternalRevision: "revision-one",
		LifecycleState: "complete", Body: "Lifecycle later",
		ExternalActorID: "actor-one", ExternalActorName: "Contributor",
		ExternalCreatedAt: later, ExternalUpdatedAt: later,
		IntegrationActor: "connector:notes",
	})
	require.NoError(t, err)
	require.True(t, created)

	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 3)
	assert.Equal(t, []string{"External earlier", "Local middle", "Lifecycle later"}, []string{
		comments[0].Body, comments[1].Body, comments[2].Body,
	})

	rows, err := store.QueryContext(ctx, `SELECT CAST(created_at AS TEXT) FROM comments WHERE issue_id=? ORDER BY id`, issue.ID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })
	var stored []string
	for rows.Next() {
		var stamp string
		require.NoError(t, rows.Scan(&stamp))
		stored = append(stored, stamp)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		middle.Format(sqliteTimeFormat),
		earlier.Format(sqliteCommentTimeFormat),
		later.Format(sqliteCommentTimeFormat),
	}, stored)
}
