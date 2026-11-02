package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/commentref"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Older-version fixtures start from the current schema and remove later indexes.
func dropCommentUIDHandleIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_comments_uid_handle`, schema)) // #nosec G201 -- Fixed test schema.
	require.NoError(t, err)
}

func TestCommentUIDHandleIndexMigrationUpgradesVersion38(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "comment_handle_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	selected, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Selected", Author: "worker"})
	require.NoError(t, err)
	moved, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Moved finding", Author: "worker"})
	require.NoError(t, err)
	finding, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: moved.ID, Author: "finder", Body: "Finding"})
	require.NoError(t, err)
	const findingUID = "01AAAAAAAAAAAAAAAAAAABCDEF"
	_, err = store.ExecContext(ctx, `UPDATE comments SET uid=$1 WHERE id=$2`, findingUID, finding.ID)
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: selected.ID, Author: "worker", Body: "Reply", ReplyToUID: findingUID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: moved.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: moved.Revision, Actor: "worker"})
	require.NoError(t, err)
	collider, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: moved.ID, Author: "finder", Body: "Collision"})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `UPDATE comments SET uid=$1 WHERE id=$2`, "01BBBBBBBBBBBBBBBBBBABCDEF", collider.ID)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropCommentUIDHandleIndex(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE comment_handle_upgrade.meta SET value='38' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	graph, err := upgraded.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: source.ID, SelectedIssueID: selected.ID})
	require.NoError(t, err)
	require.Equal(t, "moved", graph.Targets[findingUID].Status)
	require.Equal(t, "aabcdef", commentref.Handles(graph.CommentUIDsByProject[target.ID])[findingUID])
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "comment_handle_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	for _, targetSchema := range []string{schema, "comment_handle_fresh"} {
		var definition string
		require.NoError(t, admin.QueryRowContext(ctx, `SELECT replace(indexdef, schemaname || '.', '') FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_comments_uid_handle'`, targetSchema).Scan(&definition))
		require.Equal(t, `CREATE INDEX idx_comments_uid_handle ON comments USING btree (lower(substr(uid, 21)))`, definition)
	}
}
