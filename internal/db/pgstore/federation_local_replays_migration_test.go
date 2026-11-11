package pgstore_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationLocalReplaysMigrationBackfillsInterruptedReset(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "federation_replays_backfill"
	source, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	resetProject, err := source.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	pulledProject, err := source.CreateProject(ctx, "pulled-project")
	require.NoError(t, err)
	events := map[string]string{}
	var acknowledgedID int64
	for _, c := range []struct {
		name    string
		project db.Project
	}{{"unprojected", resetProject}, {"acknowledged", resetProject}, {"before", resetProject}, {"after", resetProject}, {"pulled", pulledProject}} {
		_, event, createErr := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: c.project.ID, Title: c.name, Author: "tester"})
		require.NoError(t, createErr)
		events[c.name] = event.UID
		if c.name == "acknowledged" {
			acknowledgedID = event.ID
		}
	}
	for _, project := range []db.Project{resetProject, pulledProject} {
		_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{
			ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:1",
			HubProjectID: project.ID, HubProjectUID: project.UID, ReplayHorizonEventID: 51,
			PullCursorEventID: 50, Actor: "tester", Enabled: true,
		})
		require.NoError(t, err)
		require.NoError(t, source.RecordFederationSyncReset(ctx, project.ID, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)))
	}
	require.NoError(t, source.AdvanceFederationPushCursor(ctx, resetProject.ID, acknowledgedID))
	require.NoError(t, source.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	stamp := func(eventName, createdAt string) {
		_, execErr := admin.ExecContext(ctx, `UPDATE federation_replays_backfill.events SET created_at=$1 WHERE uid=$2`, createdAt, events[eventName])
		require.NoError(t, execErr)
	}
	stamp("before", "2026-01-01T00:00:00.000Z")
	stamp("unprojected", "2026-01-01T00:00:00.000Z")
	stamp("acknowledged", "2026-01-01T00:00:00.000Z")
	stamp("after", "2026-01-03T00:00:00.000Z")
	stamp("pulled", "2026-01-01T00:00:00.000Z")
	_, err = admin.ExecContext(ctx, `UPDATE federation_replays_backfill.events SET issue_id=NULL WHERE uid=$1`, events["unprojected"])
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `DELETE FROM federation_replays_backfill.issues WHERE uid=(SELECT issue_uid FROM federation_replays_backfill.events WHERE uid=$1)`, events["unprojected"])
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `UPDATE federation_replays_backfill.federation_sync_status SET last_pull_success_at='2026-01-02T12:00:00.000Z' WHERE project_id=$1`, pulledProject.ID)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `DROP TABLE federation_replays_backfill.federation_local_replays`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `UPDATE federation_replays_backfill.meta SET value='50' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	for name, want := range map[string]bool{"unprojected": true, "acknowledged": false, "before": true, "after": false, "pulled": false} {
		got, _, replayErr := upgraded.FederationLocalReplayStatus(ctx, events[name])
		require.NoError(t, replayErr)
		require.Equal(t, want, got, name)
	}
}
