package pgstore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestCommentSearchMigrationUpgradesVersion39(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA comment_search_upgrade; SET LOCAL search_path TO comment_search_upgrade, pg_catalog, pg_temp;`+projectHubRelaySchema32)
	require.NoError(t, err)
	for _, migration := range pgstore.Migrations() {
		if migration.FromVersion >= 32 && migration.ToVersion <= 39 {
			_, err = tx.ExecContext(ctx, migration.SQL)
			require.NoError(t, err)
		}
	}
	_, err = tx.ExecContext(ctx, `
 INSERT INTO meta(key,value) VALUES('schema_version','39'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','example-project');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000002',id,'0002','login','tester' FROM projects;
 INSERT INTO comments(uid,issue_id,author,body) SELECT '00000000000000000000000003',id,'tester','safari' FROM issues;`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "comment_search_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	defer func() { _ = upgraded.Close() }()
	p, err := upgraded.ProjectByName(ctx, "example-project")
	require.NoError(t, err)
	hits, err := upgraded.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: "login safari"})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, []string{"title", "comments"}, hits[0].MatchedIn)
	var indexDefinition string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname='comment_search_upgrade' AND indexname='idx_comments_search'`).Scan(&indexDefinition))
	require.Contains(t, indexDefinition, "USING gin (to_tsvector(")
}
