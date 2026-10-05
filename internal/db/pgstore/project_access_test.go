package pgstore_test

import (
	"context"
	"database/sql"
	_ "embed"
	"sort"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestProjectAccessConformance(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, context.Background())
	t.Cleanup(cleanup)
	store, err := pgstore.Open(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.RunProjectAccessMergeIsolation(t, store)
	dbtest.RunProjectAccessConformance(t, store)
	dbtest.RunProjectAccessTokenEnrollment(t, store)
}

// Released schema at e9f580c93fc167daeb8b60d11419cabae39b8c5f.
//
//go:embed testdata/schema_v30.sql
var projectAccessSchema30 string

func TestProjectAccessMigrationUpgradesVersion30(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA access_upgrade; SET LOCAL search_path TO access_upgrade, pg_catalog, pg_temp;`+projectAccessSchema30+`
 INSERT INTO meta(key,value) VALUES('schema_version','30');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','existing-project');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000002',id,'0002','Existing task','member' FROM projects WHERE name='existing-project';`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "access_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)
	visible, err := upgraded.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{"00000000000000000000000001"}, visible)
	project, err := upgraded.ProjectByName(ctx, "existing-project")
	require.NoError(t, err)
	policy, err := upgraded.ProjectAccessPolicy(ctx, project.UID)
	require.NoError(t, err)
	require.Equal(t, "all", policy.Visibility)
	var title string
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=$1`, "00000000000000000000000002").Scan(&title))
	require.Equal(t, "Existing task", title)
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "access_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "access_fresh"), projectAccessStructure(t, upgraded, "access_upgrade"), "upgrade and canonical schemas must agree")
}

func projectAccessStructure(t *testing.T, store *pgstore.Store, schema string) []string {
	t.Helper()
	rows, err := store.QueryContext(t.Context(), `
 SELECT 'column',table_name,column_name,udt_name||':'||is_nullable||':'||COALESCE(column_default,'') FROM information_schema.columns WHERE table_schema=$1
 UNION ALL
 SELECT 'constraint',c.relname,con.conname,pg_catalog.pg_get_constraintdef(con.oid,true) FROM pg_catalog.pg_constraint con JOIN pg_catalog.pg_class c ON c.oid=con.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND con.contype<>'n'
 UNION ALL
 SELECT 'index',tablename,indexname,indexdef FROM pg_catalog.pg_indexes WHERE schemaname=$1`, schema)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var records []string
	for rows.Next() {
		var kind, table, name, definition string
		require.NoError(t, rows.Scan(&kind, &table, &name, &definition))
		records = append(records, strings.Join([]string{kind, table, name, strings.ReplaceAll(definition, schema+".", "")}, "|"))
	}
	require.NoError(t, rows.Err())
	sort.Strings(records)
	return records
}
