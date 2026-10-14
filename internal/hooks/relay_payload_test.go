package hooks

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// The daemon publishes the events relay acceptance returns, unchanged, to the
// hook dispatcher. A relayed creation's hook payload must carry the issue the
// relay materialized, as a locally created issue's payload does.
func TestRelayedIssueCreationHookPayloadCarriesIssue(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	relayed := dbtest.AcceptRelayedIssueCreation(t, store)

	issueResolver := func(ctx context.Context, id int64) (IssueSnapshot, error) {
		issue, err := store.IssueByID(ctx, id)
		if err != nil {
			return IssueSnapshot{}, err
		}
		labels, err := store.LabelsForIssue(ctx, id)
		if err != nil {
			return IssueSnapshot{}, err
		}
		return IssueSnapshot{UID: issue.UID, ShortID: issue.ShortID, Title: issue.Title, Status: issue.Status, Labels: labels, Author: issue.Author}, nil
	}
	noProject := func(context.Context, int64) (ProjectSnapshot, error) { return ProjectSnapshot{}, nil }
	noComment := func(context.Context, int64) (CommentSnapshot, error) { return CommentSnapshot{}, nil }
	noAlias := func(context.Context, db.Event) (AliasSnapshot, bool, error) { return AliasSnapshot{}, false, nil }
	logged := []string{}
	logf := func(format string, _ ...any) { logged = append(logged, format) }

	var creation *db.Event
	for i := range relayed.Accepted.InsertedEvents {
		if relayed.Accepted.InsertedEvents[i].Type == "issue.created" {
			creation = &relayed.Accepted.InsertedEvents[i]
		}
	}
	require.NotNil(t, creation)
	raw, truncated := buildStdinJSON(t.Context(), *creation, noProject, issueResolver, noComment, noAlias, logf)
	require.False(t, truncated)
	require.Empty(t, logged)
	var envelope struct {
		Issue *struct {
			UID    string   `json:"uid"`
			Title  string   `json:"title"`
			Labels []string `json:"labels"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	require.NotNil(t, envelope.Issue, "the hook payload carries an issue block")
	require.Equal(t, relayed.IssueUID, envelope.Issue.UID)
	require.Equal(t, "Relayed hook issue", envelope.Issue.Title)
	require.Equal(t, []string{"relayed"}, envelope.Issue.Labels)
}
