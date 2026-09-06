package pgstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kata/internal/uid"
)

func TestMaterializeFederatedProjectPrunesLinkedIssueMissingFromProjection(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "projection_prune", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	project, err := store.CreateProject(ctx, "projection-prune")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://projection.example", HubProjectID: 1,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	firstUID, err := uid.New()
	require.NoError(t, err)
	secondUID, err := uid.New()
	require.NoError(t, err)
	second := projectionSnapshotEvent(t, project, secondUID, origin, 1,
		json.RawMessage(`{"uid":"`+secondUID+`","title":"second","body":"","author":"remote","status":"open","metadata":{},"created_at":"2026-05-23T12:00:00.000Z"}`))
	first := projectionSnapshotEvent(t, project, firstUID, origin, 2,
		json.RawMessage(`{"uid":"`+firstUID+`","title":"first","body":"","author":"remote","status":"open","metadata":{},"links":[{"type":"blocks","to_issue_uid":"`+secondUID+`","author":"remote"}],"created_at":"2026-05-23T12:00:00.000Z"}`))
	inserted, err := store.InsertRemoteEvent(ctx, project.ID, second)
	require.NoError(t, err)
	assert.True(t, inserted)
	inserted, err = store.InsertRemoteEvent(ctx, project.ID, first)
	require.NoError(t, err)
	assert.True(t, inserted)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	firstIssue, err := store.IssueByUID(ctx, firstUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	secondIssue, err := store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	require.NoError(t, err)
	_, err = store.LinkByEndpoints(ctx, firstIssue.ID, secondIssue.ID, "blocks")
	require.NoError(t, err)

	_, err = store.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, second.EventUID)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	_, err = store.IssueByUID(ctx, secondUID, db.IncludeDeletedYes)
	assert.ErrorIs(t, err, db.ErrNotFound)
	_, err = store.LinkByEndpoints(ctx, firstIssue.ID, secondIssue.ID, "blocks")
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestMaterializeFederatedProjectLeavesUnchangedCommentsUnwritten(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := OpenWithConfig(ctx, dsn, Config{
		Schema: "comment_rebuild", SchemaMode: SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "comment-rebuild")
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: "https://hub.example", HubProjectID: 1,
		HubProjectUID: project.UID, Enabled: true,
	})
	require.NoError(t, err)
	origin, err := uid.New()
	require.NoError(t, err)
	issueUID, err := uid.New()
	require.NoError(t, err)
	commentUID, err := uid.New()
	require.NoError(t, err)
	snapshot := projectionSnapshotEvent(t, project, issueUID, origin, 1,
		json.RawMessage(`{"uid":"`+issueUID+`","title":"original issue","author":"remote","status":"open","comments":[{"comment_uid":"`+commentUID+`","author":"reviewer","body":"original comment","created_at":"2026-05-20T10:00:00.000Z"}],"created_at":"2026-05-20T09:00:00.000Z"}`))
	_, err = store.InsertRemoteEvent(ctx, project.ID, snapshot)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	var before, after string
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT xmin::text FROM comments WHERE uid=$1`, commentUID).Scan(&before))
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT xmin::text FROM comments WHERE uid=$1`, commentUID).Scan(&after))
	assert.Equal(t, before, after, "rebuilding unchanged state must not rewrite the comment row")

	edit := snapshot
	edit.EventUID, err = uid.New()
	require.NoError(t, err)
	edit.Type, edit.HLCCounter = "issue.comment_edited", 2
	edit.Payload = json.RawMessage(`{"comment_uid":"` + commentUID + `","body":"revised comment"}`)
	edit.ContentHash, err = db.EventContentHash(db.EventHashInput{
		UID: edit.EventUID, OriginInstanceUID: edit.OriginInstanceUID,
		ProjectUID: edit.ProjectUID, ProjectName: edit.ProjectName,
		IssueUID: edit.IssueUID, Type: edit.Type, Actor: edit.Actor,
		HLCPhysicalMS: edit.HLCPhysicalMS, HLCCounter: edit.HLCCounter,
		CreatedAt: "2026-05-23T12:00:00.000Z", Payload: edit.Payload,
	})
	require.NoError(t, err)
	_, err = store.InsertRemoteEvent(ctx, project.ID, edit)
	require.NoError(t, err)
	require.NoError(t, store.MaterializeFederatedProject(ctx, project.ID))
	var body string
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT body FROM comments WHERE uid=$1`, commentUID).Scan(&body))
	assert.Equal(t, "revised comment", body, "a real remote edit must still update the comment")
}

func projectionSnapshotEvent(
	t *testing.T,
	project db.Project,
	issueUID string,
	origin string,
	counter int64,
	payload json.RawMessage,
) db.RemoteEvent {
	t.Helper()
	eventUID, err := uid.New()
	require.NoError(t, err)
	createdAt := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	event := db.RemoteEvent{
		EventUID: eventUID, OriginInstanceUID: origin,
		ProjectUID: project.UID, ProjectName: project.Name, IssueUID: &issueUID,
		Type: "issue.snapshot", Actor: "remote", HLCPhysicalMS: 1779537600000,
		HLCCounter: counter, Payload: payload, CreatedAt: createdAt,
	}
	event.ContentHash, err = db.EventContentHash(db.EventHashInput{
		UID: event.EventUID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, Type: event.Type, Actor: event.Actor,
		HLCPhysicalMS: event.HLCPhysicalMS, HLCCounter: event.HLCCounter,
		CreatedAt: "2026-05-23T12:00:00.000Z", Payload: event.Payload,
	})
	require.NoError(t, err)
	return event
}
