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

func dropCommentReplySchema(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	_, err := admin.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.comments DROP COLUMN reply_to_uid, DROP COLUMN reply_kind, DROP COLUMN edited_at`, schema)) // #nosec G201 -- fixed test schema.
	require.NoError(t, err)
}

func TestCommentReplyMigrationUpgradesVersion30(t *testing.T) {
	if testing.Short() {
		t.Skip("requires PostgreSQL")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	const schema = "reply_upgrade"
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	project, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Task", Author: "worker"})
	require.NoError(t, err)
	comment, _, err := store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Existing finding"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	dropCommentReplySchema(ctx, t, admin, schema)
	_, err = admin.ExecContext(ctx, `UPDATE reply_upgrade.meta SET value='30' WHERE key='schema_version'`)
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
	require.Equal(t, 31, version)
	comments, err := upgraded.CommentsByIssue(ctx, issue.ID)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	require.Equal(t, comment.UID, comments[0].UID)
	require.Empty(t, comments[0].ReplyToUID)
	require.Empty(t, comments[0].ReplyKind)
	require.Nil(t, comments[0].EditedAt)
	_, _, err = upgraded.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "worker", Body: "Reply", ReplyToUID: comment.UID, ReplyKind: "reply"})
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO reply_upgrade.comments(uid,issue_id,author,body,reply_to_uid,reply_kind)
		VALUES($1,$2,$3,$4,$5,$6)`, "01CCCCCCCCCCCCCCCCCCCCCCCC", issue.ID, "worker", "Old kind", comment.UID, "answer")
	require.Error(t, err, "the migrated PostgreSQL CHECK constraint must reject the superseded enum")
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "reply_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	require.NoError(t, fresh.Close())
	require.Equal(t, columnFingerprint(ctx, t, admin, "reply_fresh"), columnFingerprint(ctx, t, admin, schema))
	var index string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=$1 AND indexname='idx_comments_reply_to'`, schema).Scan(&index))
	require.Contains(t, index, "WHERE (reply_to_uid IS NOT NULL)")
}
