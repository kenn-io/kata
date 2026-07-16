package pgstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestMetadataRetryClearsRolledBackAttemptOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "metadata_retry_store", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "metadata-retry")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "retry output", Author: "tester",
	})
	require.NoError(t, err)

	issuePatch := map[string]json.RawMessage{"custom": json.RawMessage(`{"ready":true}`)}
	issueOut, err := store.patchIssueMetadata(ctx, db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "tester", Patch: issuePatch,
	}, rollbackThenRetry(t, store, func() {
		_, updateErr := store.ExecContext(ctx,
			`UPDATE issues SET metadata = $1, revision = revision + 1 WHERE id = $2`,
			`{"custom":{"ready":true}}`, issue.ID)
		require.NoError(t, updateErr)
	}))
	require.NoError(t, err)
	assert.False(t, issueOut.Changed)
	assert.Zero(t, issueOut.Event.ID)
	assert.JSONEq(t, `{"custom":{"ready":true}}`, string(issueOut.Issue.Metadata))

	projectOut, err := store.patchProjectMetadata(ctx, db.PatchProjectMetadataIn{
		ProjectID: project.ID, Actor: "tester",
		Patch: map[string]json.RawMessage{"area": json.RawMessage(`"runtime"`)},
	}, rollbackThenRetry(t, store, func() {
		_, updateErr := store.ExecContext(ctx,
			`UPDATE projects SET metadata = $1, revision = revision + 1 WHERE id = $2`,
			`{"area":"runtime"}`, project.ID)
		require.NoError(t, updateErr)
	}))
	require.NoError(t, err)
	assert.False(t, projectOut.Changed)
	assert.Zero(t, projectOut.Event.ID)
	assert.JSONEq(t, `{"area":"runtime"}`, string(projectOut.Project.Metadata))
}

func rollbackThenRetry(
	t *testing.T,
	store *Store,
	between func(),
) metadataTxRunner {
	t.Helper()
	return func(ctx context.Context, operation transactionFunc) error {
		first, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}
		if err := operation(first); err != nil {
			_ = first.Rollback()
			return err
		}
		if err := first.Rollback(); err != nil {
			return err
		}
		between()
		second, err := store.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			return err
		}
		defer func() { _ = second.Rollback() }()
		if err := operation(second); err != nil {
			return err
		}
		return second.Commit()
	}
}
