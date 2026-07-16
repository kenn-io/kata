package pgstore_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestImportReplayFreshTargetRejectsConcurrentWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	writer, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	writerTx, err := writer.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writerTx.Rollback() })
	const writerUID = "01KATA00000000000000000001"
	_, err = writerTx.ExecContext(ctx, `
		INSERT INTO kata.projects(uid, name)
		VALUES($1, 'concurrent-project')`, writerUID)
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		result <- store.ImportReplay(ctx, []db.ImportRecord{{
			Kind: db.ImportKindProject,
			Project: &db.ProjectExport{
				ID: 3, UID: "01KATA00000000000000000002", Name: "restored-project",
				CreatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
			},
		}}, db.ImportOptions{RequireFreshTarget: true})
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := writer.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_catalog.pg_locks AS lock
				JOIN pg_catalog.pg_class AS relation ON relation.oid = lock.relation
				JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace
				WHERE namespace.nspname = 'kata'
				  AND relation.relname = 'projects'
				  AND lock.mode = 'AccessExclusiveLock'
				  AND NOT lock.granted
			)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "replay never waited for the concurrent writer")

	require.NoError(t, writerTx.Commit())
	select {
	case err := <-result:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fresh target")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "replay remained blocked after the writer committed")
	}

	preserved, err := store.ProjectByUID(ctx, writerUID)
	require.NoError(t, err)
	assert.Equal(t, "concurrent-project", preserved.Name)
}

func TestImportReplayWaitsForConcurrentSchemaMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	migrator, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = migrator.Close() })
	migrationTx, err := migrator.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = migrationTx.Rollback() })
	_, err = migrationTx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('kata:pgstore:migrations', 0))`)
	require.NoError(t, err)
	_, err = migrationTx.ExecContext(ctx, `CREATE TABLE kata.late_state(value TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = migrationTx.ExecContext(ctx, `INSERT INTO kata.late_state(value) VALUES('must-be-cleared')`)
	require.NoError(t, err)

	result := make(chan error, 1)
	go func() {
		result <- store.ImportReplay(ctx, []db.ImportRecord{{
			Kind: db.ImportKindProject,
			Project: &db.ProjectExport{
				ID: 2, UID: "01KATA00000000000000000003", Name: "restored-project",
				CreatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
			},
		}}, db.ImportOptions{})
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := migrator.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_locks
				WHERE locktype = 'advisory' AND NOT granted
			)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "replay did not wait for the schema migration lock")

	require.NoError(t, migrationTx.Commit())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "replay remained blocked after the migration committed")
	}

	var retainedRows int
	require.NoError(t, migrator.QueryRowContext(ctx, `SELECT COUNT(*) FROM kata.late_state`).Scan(&retainedRows))
	assert.Zero(t, retainedRows, "replay must clear tables committed by a concurrent migration")
}

func TestImportReplayWaitsForServingDaemonAcrossConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	serving, err := pgstore.Open(ctx, dsn, db.Serving())
	require.NoError(t, err)
	restore, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = restore.Close() })

	result := make(chan error, 1)
	go func() {
		result <- restore.ImportReplay(ctx, []db.ImportRecord{{
			Kind: db.ImportKindMeta,
			Meta: &db.MetaKV{Key: "instance_uid", Value: "01KATA00000000000000000044"},
		}}, db.ImportOptions{})
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		err := restore.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_locks
				WHERE locktype = 'advisory' AND mode = 'ExclusiveLock' AND NOT granted
			)`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "restore did not wait for the serving lease")

	require.NoError(t, serving.Close())
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "restore remained blocked after serving daemon stopped")
	}
	require.NoError(t, restore.RefreshInstanceUID(ctx))
	assert.Equal(t, "01KATA00000000000000000044", restore.InstanceUID())
}

func TestImportReplayRefusesCrossSchemaCascade(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "referenced-project")
	require.NoError(t, err)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `CREATE SCHEMA unrelated`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `CREATE TABLE unrelated.project_refs(
			project_id bigint PRIMARY KEY REFERENCES kata.projects(id),
			note text NOT NULL
		)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx,
		`INSERT INTO unrelated.project_refs(project_id,note) VALUES($1,'must survive')`, project.ID)
	require.NoError(t, err)

	err = store.ImportReplay(ctx, []db.ImportRecord{{
		Kind: db.ImportKindProject,
		Project: &db.ProjectExport{
			ID: 9, UID: "01KATA00000000000000000009", Name: "replacement",
			CreatedAt: "2026-07-15T12:00:00.000Z", Metadata: json.RawMessage(`{}`), Revision: 1,
		},
	}}, db.ImportOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "clear import target")

	var note string
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT note FROM unrelated.project_refs WHERE project_id=$1`, project.ID).Scan(&note))
	assert.Equal(t, "must survive", note)
	_, err = store.ProjectByID(ctx, project.ID)
	require.NoError(t, err, "failed replacement must roll back kata-owned state")
}

func TestImportReplayClearsTargetOnlyMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES('claim_status_refresh_error:old','stale')`)
	require.NoError(t, err)

	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{{
		Kind: db.ImportKindMeta,
		Meta: &db.MetaKV{Key: "instance_uid", Value: "01KATA00000000000000000010"},
	}}, db.ImportOptions{}))
	var stale string
	err = store.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='claim_status_refresh_error:old'`).Scan(&stale)
	assert.ErrorIs(t, err, sql.ErrNoRows)
	assert.Equal(t, "01KATA00000000000000000010", store.InstanceUID())

	localUID := store.InstanceUID()
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{{
		Kind: db.ImportKindMeta,
		Meta: &db.MetaKV{Key: "instance_uid", Value: "01KATA00000000000000000011"},
	}}, db.ImportOptions{NewInstance: true}))
	assert.Equal(t, localUID, store.InstanceUID())
}

func TestImportReplayPreservesHistoricalEventProjectName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	const (
		projectUID  = "01KATA00000000000000000012"
		eventUID    = "01KATA00000000000000000013"
		instanceUID = "01KATA00000000000000000014"
		createdAt   = "2026-07-15T12:00:00.000Z"
	)
	payload := json.RawMessage(`{"name":"old-name"}`)
	hash, err := db.EventContentHash(db.EventHashInput{
		UID: eventUID, OriginInstanceUID: instanceUID, ProjectUID: projectUID,
		ProjectName: "old-name", Type: "project.created", Actor: "operator",
		HLCPhysicalMS: 1784116800000, CreatedAt: createdAt, Payload: payload,
	})
	require.NoError(t, err)
	require.NoError(t, store.ImportReplay(ctx, []db.ImportRecord{
		{Kind: db.ImportKindMeta, Meta: &db.MetaKV{Key: "instance_uid", Value: instanceUID}},
		{Kind: db.ImportKindProject, Project: &db.ProjectExport{
			ID: 12, UID: projectUID, Name: "new-name", CreatedAt: createdAt,
			Metadata: json.RawMessage(`{}`), Revision: 1,
		}},
		{Kind: db.ImportKindEvent, Event: &db.EventExport{
			ID: 13, UID: eventUID, OriginInstanceUID: instanceUID, ProjectID: 12,
			ProjectUID: projectUID, ProjectName: "old-name", Type: "project.created",
			Actor: "operator", Payload: payload, HLCPhysicalMS: 1784116800000,
			ContentHash: hash, CreatedAt: createdAt,
		}},
	}, db.ImportOptions{}))
	var storedName string
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT project_name FROM events WHERE uid=$1`, eventUID).Scan(&storedName))
	assert.Equal(t, "old-name", storedName)
}
