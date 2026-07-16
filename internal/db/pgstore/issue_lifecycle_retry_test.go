package pgstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestCloseIssueRetryClearsRolledBackAttemptOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "lifecycle_retry_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "lifecycle-retry")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "retry close", Author: "tester",
	})
	require.NoError(t, err)

	closedAt := mutationTimestamp()
	closed, events, changed, err := store.closeIssueWithEvents(
		ctx, issue.ID, "done", "tester", "", nil,
		rollbackThenRetry(t, store, func() {
			_, updateErr := store.ExecContext(ctx, `UPDATE issues
SET status='closed', closed_reason='done', closed_at=$1, updated_at=$1 WHERE id=$2`,
				closedAt, issue.ID)
			require.NoError(t, updateErr)
		}),
	)
	require.NoError(t, err)
	assert.Equal(t, "closed", closed.Status)
	assert.False(t, changed)
	assert.Empty(t, events, "rolled-back close events must not escape a successful no-op retry")
}

func TestArchivedProjectRejectsIssueMutation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "archived_mutation_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "archived-mutation")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "original title", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "tester", Force: true,
	})
	require.NoError(t, err)

	updatedTitle := "must not be written"
	_, event, changed, err := store.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &updatedTitle, Actor: "tester",
	})
	assert.ErrorIs(t, err, db.ErrNotFound)
	assert.Nil(t, event)
	assert.False(t, changed)

	var storedTitle string
	err = store.QueryRowContext(ctx, `SELECT title FROM issues WHERE id=$1`, issue.ID).Scan(&storedTitle)
	require.NoError(t, err)
	assert.Equal(t, "original title", storedTitle)
}
