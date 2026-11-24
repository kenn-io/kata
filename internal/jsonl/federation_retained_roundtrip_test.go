package jsonl_test

import (
	"bytes"
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/testenv"
)

func TestFederationRetainedEventsRoundTrip(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			ctx := t.Context()
			var source, target db.Storage
			if backend == "sqlite" {
				source, target = openExportTestDB(t), openImportTargetDB(t)
			} else {
				dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
				t.Cleanup(cleanup)
				var err error
				source, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "retained_source", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, source.Close()) })
				target, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "retained_target", SchemaMode: pgstore.SchemaModeBootstrap})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, target.Close()) })
			}
			project, err := source.CreateProject(ctx, "rejoin-project")
			require.NoError(t, err)
			_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 1, HubProjectUID: project.UID, Actor: "actor", Enabled: true, PushEnabled: true})
			require.NoError(t, err)
			_, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "local task", Author: "actor"})
			require.NoError(t, err)
			sqlStore := source.(interface {
				ExecContext(context.Context, string, ...any) (sql.Result, error)
			})
			_, err = sqlStore.ExecContext(ctx, `INSERT INTO federation_retained_events(event_id,kept_local) VALUES($1,1)`, event.ID)
			require.NoError(t, err)
			var data bytes.Buffer
			require.NoError(t, jsonl.Export(ctx, source, &data, jsonl.ExportOptions{}))
			require.NoError(t, jsonl.Import(ctx, &data, target))
			var retained []db.FederationRetainedEventExport
			for rec, err := range target.ExportFederationRetainedEvents(ctx, db.ExportFilter{}) {
				require.NoError(t, err)
				retained = append(retained, rec)
			}
			require.Equal(t, []db.FederationRetainedEventExport{{EventUID: event.UID, KeptLocal: 1}}, retained)
		})
	}
}

func TestFederationWaitingEventsSurviveNewInstanceReset(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			testFederationWaitingEventsSurviveNewInstanceReset(t, backend)
		})
	}
}

func testFederationWaitingEventsSurviveNewInstanceReset(t *testing.T, backend string) {
	t.Helper()
	ctx := t.Context()
	var source, target db.Storage
	if backend == "sqlite" {
		source, target = openExportTestDB(t), openImportTargetDB(t)
	} else {
		dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
		t.Cleanup(cleanup)
		var err error
		source, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "waiting_source", SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, source.Close()) })
		target, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "waiting_target", SchemaMode: pgstore.SchemaModeBootstrap})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, target.Close()) })
	}
	project, err := source.CreateProject(ctx, "rejoin-project")
	require.NoError(t, err)
	_, err = source.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: "http://127.0.0.1:7373", HubProjectID: 1, HubProjectUID: project.UID, Actor: "actor", Enabled: true, PushEnabled: true})
	require.NoError(t, err)
	_, event, err := source.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "waiting task", Author: "actor"})
	require.NoError(t, err)
	sqlStore := source.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = sqlStore.ExecContext(ctx, `INSERT INTO federation_retained_events(event_id,kept_local) VALUES($1,0)`, event.ID)
	require.NoError(t, err)
	var data bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, source, &data, jsonl.ExportOptions{}))
	require.NoError(t, jsonl.ImportWithOptions(ctx, &data, target, jsonl.ImportOptions{NewInstance: true}))
	binding, err := target.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.NoError(t, target.ResetFederatedProjectIfNoPendingPush(ctx, project.ID, 1, 0, target.InstanceUID(), binding.PushCursorEventID))
	got, err := target.EventsByUIDs(ctx, project.ID, []string{event.UID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, event.ContentHash, got[0].ContentHash)
}
