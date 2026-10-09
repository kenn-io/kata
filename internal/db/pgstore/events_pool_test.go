package pgstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestEventsAfterResolvesScopedReferencesWithOneConnection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "event_scope_pool", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	visible, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hidden, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	hiddenIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: hidden.ID, Title: "Restricted peer", Author: "example-actor",
	})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: visible.ID, Title: "Visible issue", Author: "example-actor",
		Links: []db.InitialLink{{Type: "related", ToNumber: hiddenIssue.ID, ExpectedProjectUID: hidden.UID}},
	})
	require.NoError(t, err)

	store.SetMaxOpenConns(1)
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	events, err := store.EventsAfter(db.WithAuthorizedProjects(bounded, []string{visible.UID}), db.EventsAfterParams{Limit: 100})
	require.NoError(t, err)
	require.NotEmpty(t, events)
	for _, event := range events {
		if event.Type == db.ProjectScopeResetEventType {
			require.Empty(t, event.Payload)
			require.Nil(t, event.IssueUID)
		}
	}
}
