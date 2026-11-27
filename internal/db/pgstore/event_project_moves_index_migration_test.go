package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// dropSchema53ProjectMovesIndex returns a fresh schema's indexes to their
// schema 52 shape, including the later schema 56 issue metadata index. Older
// fixtures reach it through restoreSchema50EventIndex; it does nothing once
// the indexes are gone.
func dropSchema53ProjectMovesIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	dropSchema56IssueMetadataIndex(ctx, t, admin, schema)
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_events_project_moves`, schema)) // #nosec G201 -- schema is a fixed test identifier.
	require.NoError(t, err)
}

// projectMovesIndex returns the definition of idx_events_project_moves in
// schema, or "" when the schema has none.
func projectMovesIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) string {
	t.Helper()
	var definition sql.NullString
	err := admin.QueryRowContext(ctx, `SELECT replace(indexdef,$1||'.','') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_events_project_moves'`, schema).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	require.NoError(t, err)
	return definition.String
}

// Contract: schema 52 upgrades to 53 by adding the index of move events by
// destination project, matching a fresh install, and a federation read still
// reports that a project with a moved-in issue requires move support.
func TestEventProjectMovesIndexMigrationUpgradesVersion52(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "project_moves_index_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	source, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved", Author: "worker"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "worker"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, target.ID, "worker")
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropSchema53ProjectMovesIndex(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE project_moves_index_upgrade.meta SET value='52' WHERE key='schema_version'`)
	require.NoError(t, err)

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	read, err := upgraded.ReadFederation(ctx, db.FederationReadParams{ProjectID: target.ID, Features: db.SupportedEventFeatures, Limit: 10})
	require.NoError(t, err)
	assert.Contains(t, read.RequiredFeatures, db.IssueMoveEventFeature)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "project_moves_index_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	migrated := projectMovesIndex(ctx, t, admin, schema)
	assert.Equal(t, projectMovesIndex(ctx, t, admin, "project_moves_index_fresh"), migrated, "migrated and fresh schemas must agree")
	assert.Equal(t, "CREATE INDEX idx_events_project_moves ON events USING btree (project_id, id) WHERE (type = 'issue.moved'::text)", migrated)
}
