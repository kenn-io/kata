package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkMetadataPatchTransactionHook(t *testing.T, store db.Storage) error {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "metadata-hook-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Notification", Author: "worker"})
	require.NoError(t, err)
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)

	patch := map[string]jsontext.Value{"notify.worker": jsontext.Value(`{"from":"lead","message":"Inspect this finding"}`)}
	hookCalls := 0
	hook := func(ctx context.Context, tx *sql.Tx, current db.Issue) (map[string]jsontext.Value, error) {
		hookCalls++
		require.Equal(t, issue.UID, current.UID)
		var one int
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one))
		require.Equal(t, 1, one)
		return patch, nil
	}
	patched, err := store.PatchIssueMetadata(db.WithMetadataPatchHook(ctx, hook), db.PatchIssueMetadataIn{
		IssueID: issue.ID, Actor: "lead",
	})
	require.NoError(t, err)
	require.Equal(t, 1, hookCalls)
	require.True(t, patched.Changed, "the transaction hook's patch must be applied")
	require.Equal(t, "issue.metadata_updated", patched.Event.Type)
	require.Equal(t, "lead", patched.Event.Actor)
	require.Contains(t, patched.Issue.Metadata, "Inspect this finding")

	events, err := store.EventsAfter(ctx, db.EventsAfterParams{AfterID: before, ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, patched.Event.UID, events[0].UID, "the resolved patch event must be the committed event")
	return nil
}
