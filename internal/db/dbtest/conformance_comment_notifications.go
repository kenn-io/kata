package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkCommentNotifications(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	p, e := store.CreateProject(ctx, "example-project")
	require.NoError(t, e)
	issue, _, e := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "Finding", Author: "worker"})
	require.NoError(t, e)
	_, e = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value("42")}})
	require.ErrorContains(t, e, "scheduled_on", "validation errors identify the rejected key")
	var events []db.Event
	hook := func(_ context.Context, tx *sql.Tx, _ db.Issue, comment db.Comment) (map[string]jsontext.Value, error) {
		var count int
		require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM comments WHERE uid=$1", comment.UID).Scan(&count))
		require.Equal(t, 1, count)
		value, e := json.Marshal(map[string]any{"from": "worker", "message": "reply", "re": comment.UID, "kind": "reply"})
		return map[string]jsontext.Value{"notify.cmVhZGVy": value}, e
	}
	c, event, e := store.CreateComment(db.WithCommentMetadataHook(ctx, hook, &events), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Evidence"})
	require.NoError(t, e)
	require.Len(t, events, 2)
	require.Equal(t, event.UID, events[0].UID)
	require.Equal(t, "issue.metadata_updated", events[1].Type)
	current, e := store.IssueByID(ctx, issue.ID)
	require.NoError(t, e)
	require.Equal(t, issue.Revision+1, current.Revision)
	require.Contains(t, string(current.Metadata), c.UID)
	// Simulate an older reader: discard all new commented fields and apply only
	// the ordinary metadata event. No automatic notification derivation occurs.
	var payload struct {
		Diff map[string]struct {
			To jsontext.Value `json:"to"`
		} `json:"diff"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[1].Payload), &payload))
	patch := map[string]jsontext.Value{}
	for key, d := range payload.Diff {
		patch[key] = d.To
	}
	replay, e := db.ApplyMetadataPatch(nil, patch)
	require.NoError(t, e)
	require.JSONEq(t, string(current.Metadata), string(replay))
	require.NotContains(t, event.Payload, "notify.")
	events = nil
	_, _, e = store.CreateComment(db.WithCommentMetadataHook(ctx, func(context.Context, *sql.Tx, db.Issue, db.Comment) (map[string]jsontext.Value, error) {
		return nil, errors.New("policy refused")
	}, &events), db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Rejected"})
	require.EqualError(t, e, "policy refused")
	require.Empty(t, events)
	comments, e := store.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, e)
	require.Len(t, comments, 1)
	return nil
}
