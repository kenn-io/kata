package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func dropIssueStatusSchema(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	dropNativeCronSchema(ctx, t, admin, schema)
	// Reconstruct the released schema-29 table shape from the current DDL.
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.import_mappings
		DROP COLUMN IF EXISTS observed_status,
		DROP COLUMN IF EXISTS observed_status_at,
		DROP COLUMN IF EXISTS pending_event_uid,
		DROP COLUMN IF EXISTS remote_locator`, schema)) // #nosec G201 -- schema is a fixed test identifier.
	require.NoError(t, err)
}

func TestIssueStatusMigrationUpgradesVersion29(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "status_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Existing task", Author: "worker"})
	require.NoError(t, err)
	var mappingID int64
	err = store.QueryRowContext(ctx, `INSERT INTO import_mappings
		(source, external_id, object_type, project_id, issue_id)
		VALUES ('notion:example-source', 'page-1', 'issue', $1, $2) RETURNING id`, project.ID, issue.ID).Scan(&mappingID)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropIssueStatusSchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE status_upgrade.meta SET value='29' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.Equal(t, "f46458bfdd656755b4921c995d8785dcd43033626ef0dc5bee6e7e4d5c1d12cf", columnFingerprint(ctx, t, admin, schema))
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	var status, observedAt, pending, locator sql.NullString
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT observed_status, observed_status_at, pending_event_uid, remote_locator FROM import_mappings WHERE id=$1`, mappingID).Scan(&status, &observedAt, &pending, &locator))
	require.False(t, status.Valid)
	require.False(t, observedAt.Valid)
	require.False(t, pending.Valid)
	require.False(t, locator.Valid)
	retained, err := upgraded.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.Equal(t, issue.UID, retained.UID)
	_, err = upgraded.ExecContext(ctx, `UPDATE import_mappings SET observed_status_at='2026-09-29T12:00:00Z' WHERE id=$1`, mappingID)
	require.NoError(t, err)
	require.NoError(t, upgraded.Close())
	reopened, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err, "migration is not reapplied on a current database")
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(t, reopened.QueryRowContext(ctx, `SELECT observed_status, observed_status_at FROM import_mappings WHERE id=$1`, mappingID).Scan(&status, &observedAt))
	require.False(t, status.Valid)
	require.Equal(t, sql.NullString{String: "2026-09-29T12:00:00Z", Valid: true}, observedAt)
}
