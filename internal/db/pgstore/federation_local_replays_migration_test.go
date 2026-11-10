package pgstore_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationLocalReplaysMigration(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "federation_replays_upgrade"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "local event", Author: "tester"})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `DROP TABLE federation_replays_upgrade.federation_local_replays`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `UPDATE federation_replays_upgrade.meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	var count int
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT COUNT(*) FROM federation_local_replays`).Scan(&count))
	require.Zero(t, count)
	_, err = upgraded.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, event.UID)
	require.NoError(t, err)
	inserted, err := upgraded.InsertRemoteEvent(ctx, project.ID, db.RemoteEvent{
		EventUID: event.UID, OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID: event.ProjectUID, ProjectName: event.ProjectName,
		IssueUID: event.IssueUID, RelatedIssueUID: event.RelatedIssueUID,
		Type: event.Type, Actor: event.Actor, HLCPhysicalMS: event.HLCPhysicalMS,
		HLCCounter: event.HLCCounter, ContentHash: event.ContentHash,
		Payload: jsontext.Value(event.Payload), CreatedAt: event.CreatedAt,
	})
	require.NoError(t, err)
	require.True(t, inserted)
	isReplay, err := upgraded.IsFederationLocalReplay(ctx, event.UID)
	require.NoError(t, err)
	require.True(t, isReplay)
	_, err = upgraded.ExecContext(ctx, `DELETE FROM events WHERE uid=$1`, event.UID)
	require.NoError(t, err)
	isReplay, err = upgraded.IsFederationLocalReplay(ctx, event.UID)
	require.NoError(t, err)
	require.False(t, isReplay)
}
