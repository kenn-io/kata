package pgstore_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Prior-version migration fixtures restore the aggregate search triggers.
func restoreCommentSearch39(ctx context.Context, t *testing.T, admin *sql.DB, schema string) {
	t.Helper()
	start := strings.Index(projectHubRelaySchema32, "CREATE OR REPLACE FUNCTION rebuild_issue_search")
	require.NotEqual(t, -1, start)
	searchSQL, _, found := strings.Cut(projectHubRelaySchema32[start:], "\n-- ---")
	require.True(t, found)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`SET LOCAL search_path TO %s, public;
 DROP INDEX IF EXISTS idx_comments_search;
 DROP TRIGGER issues_search_after_issue_insert ON issues;
 DROP TRIGGER issues_search_after_issue_update ON issues;
 DROP TRIGGER IF EXISTS issues_search_after_comment_insert ON comments;
 DROP TRIGGER IF EXISTS issues_search_after_comment_update ON comments;
 DROP TRIGGER IF EXISTS issues_search_after_comment_delete ON comments;
 `, schema)+searchSQL+`SELECT rebuild_issue_search(id) FROM issues;`) // #nosec G201 -- Fixed test schema.
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestCommentSearchMigrationUpgradesVersion39(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "comment_search_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	p, err := store.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "login", Author: "tester"})
	require.NoError(t, err)
	_, _, err = store.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "safari"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer func() { _ = admin.Close() }()
	restoreCommentSearch39(ctx, t, admin, "comment_search_upgrade")
	_, err = admin.ExecContext(ctx, `UPDATE comment_search_upgrade.meta SET value='39' WHERE key='schema_version'`)
	require.NoError(t, err)
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "comment_search_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	defer func() { _ = upgraded.Close() }()
	hits, err := upgraded.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login safari"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, []string{"title", "comments"}, hits[0].MatchedIn)
	var indexDefinition string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='comment_search_upgrade' AND indexname='idx_comments_search'`).Scan(&indexDefinition))
	require.Contains(t, indexDefinition, "USING gin (to_tsvector(")
}
