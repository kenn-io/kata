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

func TestIssueStatusMigrationUpgradesVersion30(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "status_columns_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Existing task", Author: "worker"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropIssueStatusSchema(ctx, t, admin, schema)
	// Recreate the historical column using the unchanged v30 asset, then pin
	// the fixture's catalog to the actual v30 column manifest.
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO status_columns_upgrade, public`)
	require.NoError(t, err)
	for _, migration := range pgstore.Migrations() {
		if migration.ToVersion == 30 {
			_, err = tx.ExecContext(ctx, migration.SQL)
			require.NoError(t, err)
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE meta SET value='30' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Equal(t, "ef6b96e805b6be863be9f6dc3760d0626c4d5255e1fb1e6312bb346f7e74848a", columnFingerprint(ctx, t, admin, schema))
	const observedAt = "2026-09-29T12:00:00.123Z"
	const pendingUID = "01HZZZZZZZZZZZZZZZZZZZZZ13"
	fixtures := []struct {
		name, checkpoint, status, at, pending, locator string
	}{
		{name: "absent"},
		{name: "empty", checkpoint: `{}`},
		{name: "null_status", checkpoint: `{"observed":{"raw":null,"version":"2026-09-29T12:00:00.123Z"}}`, at: observedAt},
		{name: "option", checkpoint: `{"observed":{"raw":"option-complete","version":"2026-09-29T12:00:00.123Z"}}`, status: "option-complete", at: observedAt},
		{name: "pending", checkpoint: fmt.Sprintf(`{"pending_event_uid":%q}`, pendingUID), pending: pendingUID},
		{name: "github", checkpoint: fmt.Sprintf(`{"observed":{"raw":"closed","version":"2026-09-29T12:00:00.123Z"},"pending_event_uid":%q,"github_issue_number":42}`, pendingUID), status: "closed", at: observedAt, pending: pendingUID, locator: "42"},
	}
	for _, fixture := range fixtures {
		var checkpoint any
		if fixture.checkpoint != "" {
			checkpoint = fixture.checkpoint
		}
		_, err = admin.ExecContext(ctx, `INSERT INTO status_columns_upgrade.import_mappings
			(source, external_id, object_type, project_id, issue_id, source_updated_at, imported_at, status_sync_json)
			VALUES ('example-source', $1, 'issue', $2, $3, $4, $4, $5)`, fixture.name, project.ID, issue.ID, observedAt, checkpoint)
		require.NoError(t, err)
	}
	for _, phase := range []string{"upgrade", "reopen"} {
		t.Run(phase, func(t *testing.T) {
			upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
			require.NoError(t, err)
			t.Cleanup(func() { _ = upgraded.Close() })
			version, err := upgraded.SchemaVersion(ctx)
			require.NoError(t, err)
			require.Equal(t, 31, version)
			for _, fixture := range fixtures {
				t.Run(fixture.name, func(t *testing.T) {
					var status, at, pending, locator sql.NullString
					var projectID, issueID int64
					var source, sourceAt, importedAt string
					require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT observed_status, observed_status_at,
						pending_event_uid, remote_locator, project_id, issue_id, source, source_updated_at, imported_at
						FROM import_mappings WHERE external_id=$1`, fixture.name).Scan(&status, &at, &pending, &locator, &projectID, &issueID, &source, &sourceAt, &importedAt))
					require.Equal(t, sql.NullString{String: fixture.status, Valid: fixture.status != ""}, status)
					require.Equal(t, sql.NullString{String: fixture.at, Valid: fixture.at != ""}, at)
					require.Equal(t, sql.NullString{String: fixture.pending, Valid: fixture.pending != ""}, pending)
					require.Equal(t, sql.NullString{String: fixture.locator, Valid: fixture.locator != ""}, locator)
					require.Equal(t, project.ID, projectID)
					require.Equal(t, issue.ID, issueID)
					require.Equal(t, "example-source", source)
					require.Equal(t, observedAt, sourceAt)
					require.Equal(t, observedAt, importedAt)
				})
			}
			require.NoError(t, upgraded.Close())
		})
	}
}

func dropIssueStatusSchema(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	// Test-local reconstruction supports the pre-change and post-change
	// canonical schemas so the upgrade test can fail before implementation.
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.import_mappings
		DROP COLUMN IF EXISTS status_sync_json,
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
	require.Equal(t, 31, version)
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
