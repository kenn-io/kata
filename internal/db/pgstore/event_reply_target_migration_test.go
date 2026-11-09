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

// Older-version fixtures must remove the reply target index from fresh schemas.
func dropEventReplyTargetIndex(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s.idx_events_reply_to`, schema)) // #nosec G201 -- fixed test schema.
	require.NoError(t, err)
}

// Upgrading preserves removed-target evidence and matches a fresh install.
func TestEventReplyTargetIndexMigrationUpgradesVersion35(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "reply_target_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	source, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Reply source", Author: "worker"})
	require.NoError(t, err)
	target, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Reply target", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: target.ID, Author: "worker", Body: "Finding"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: source.ID, Author: "worker", Body: "Reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, target.ID, "worker", nil)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropEventReplyTargetIndex(ctx, t, admin, schema)
	dropLaterMigrationIndexes(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE reply_target_upgrade.meta SET value='35' WHERE key='schema_version'`)
	require.NoError(t, err)
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaOwner: owner, SchemaMode: pgstore.SchemaModeValidate})
	require.Error(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	graph, err := upgraded.ReadCommentGraph(ctx, db.CommentGraphQuery{ProjectID: project.ID, SelectedIssueID: source.ID})
	require.NoError(t, err)
	require.Equal(t, "removed", graph.Targets[comment.UID].Status)
}
