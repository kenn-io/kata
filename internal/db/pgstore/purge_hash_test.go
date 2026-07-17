package pgstore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestPurgeRehashesRetainedEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	t.Run("issue peer reference", func(t *testing.T) {
		store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
			Schema: "purge_issue_hash", SchemaMode: pgstore.SchemaModeBootstrap,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })

		project, err := store.CreateProject(ctx, "issue-purge-hash")
		require.NoError(t, err)
		subject, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "subject", Author: "tester",
		})
		require.NoError(t, err)
		peer, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "peer", Author: "tester",
		})
		require.NoError(t, err)
		_, linkEvent, err := store.CreateLinkAndEvent(ctx, db.CreateLinkParams{
			FromIssueID: subject.ID, ToIssueID: peer.ID, Type: "blocks", Author: "tester",
		}, db.LinkEventParams{
			EventType: "issue.links_changed", EventIssueID: subject.ID,
			FromShortID: subject.ShortID, FromUID: subject.UID,
			ToShortID: peer.ShortID, ToUID: peer.UID, Actor: "tester",
		})
		require.NoError(t, err)
		require.NotNil(t, linkEvent.RelatedIssueUID)

		_, err = store.PurgeIssue(ctx, peer.ID, "tester", nil)
		require.NoError(t, err)
		stored := eventByUID(ctx, t, store, project.ID, linkEvent.UID)
		require.Nil(t, stored.RelatedIssueUID)
		requireEventHashValid(t, stored)
	})

	t.Run("project issue reference", func(t *testing.T) {
		store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
			Schema: "purge_project_hash", SchemaMode: pgstore.SchemaModeBootstrap,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, store.Close()) })

		origin, err := store.CreateProject(ctx, "hash-origin")
		require.NoError(t, err)
		destination, err := store.CreateProject(ctx, "hash-destination")
		require.NoError(t, err)
		issue, created, err := store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: origin.ID, Title: "move then purge", Author: "tester",
		})
		require.NoError(t, err)
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: origin.ID, ToProjectID: destination.ID,
			IfMatchRev: issue.Revision, Actor: "tester",
		})
		require.NoError(t, err)
		_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
			ProjectID: destination.ID, Actor: "tester", Force: true,
		})
		require.NoError(t, err)
		_, err = store.PurgeProject(ctx, db.PurgeProjectParams{ProjectID: destination.ID, Actor: "tester"})
		require.NoError(t, err)

		stored := eventByUID(ctx, t, store, origin.ID, created.UID)
		require.Nil(t, stored.IssueUID)
		requireEventHashValid(t, stored)
	})
}

func eventByUID(ctx context.Context, t *testing.T, store *pgstore.Store, projectID int64, uid string) db.Event {
	t.Helper()
	events, err := store.EventsByUIDs(ctx, projectID, []string{uid})
	require.NoError(t, err)
	require.Len(t, events, 1)
	return events[0]
}

func requireEventHashValid(t *testing.T, event db.Event) {
	t.Helper()
	_, _, err := db.ValidateRemoteEventContentHash(db.RemoteEvent{
		EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
		Payload: json.RawMessage(event.Payload), CreatedAt: event.CreatedAt,
	})
	require.NoError(t, err)
}
