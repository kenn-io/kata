package pgstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCommentsOrderMixedPrecisionTimestamps(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "comment_ordering", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "Timestamp ordering", Author: "tester",
	})
	require.NoError(t, err)

	bodies := []string{"Earlier", "Middle", "Later"}
	stamps := []string{
		"2026-08-20T10:00:00.122900000Z",
		"2026-08-20T10:00:00.123Z",
		"2026-08-20T10:00:00.123100000Z",
	}
	for i := range bodies {
		comment, _, createErr := store.CreateComment(ctx, db.CreateCommentParams{
			IssueID: issue.ID, Author: "tester", Body: bodies[i],
		})
		require.NoError(t, createErr)
		_, updateErr := store.ExecContext(ctx,
			`UPDATE comments SET created_at=$1 WHERE id=$2`, stamps[i], comment.ID)
		require.NoError(t, updateErr)
	}

	comments, err := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, bodies, commentBodies(comments))

	tx, err := store.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	uiComments, err := readUIComments(ctx, tx, issue.ID)
	require.NoError(t, err)
	assert.Equal(t, bodies, commentBodies(uiComments))
}

func commentBodies(comments []db.Comment) []string {
	bodies := make([]string, len(comments))
	for i := range comments {
		bodies[i] = comments[i].Body
	}
	return bodies
}
