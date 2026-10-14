package pgstore_test

import (
	"database/sql"
	_ "embed"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

// Released schema 32: main with comment replies, before project hub relay.
//
//go:embed testdata/schema_v32.sql
var projectHubRelaySchema32 string

func TestProjectHubRelayMigrationUpgradesVersion32(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA relay_upgrade; SET LOCAL search_path TO relay_upgrade, pg_catalog, pg_temp;`+projectHubRelaySchema32+`
 INSERT INTO meta(key,value) VALUES('schema_version','32'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','existing-project');
 INSERT INTO issues(uid,project_id,short_id,title,author) SELECT '00000000000000000000000002',id,'0002','Existing task','member' FROM projects WHERE name='existing-project';
 INSERT INTO api_tokens(token_hash,actor,name) VALUES(repeat('a',64),'member','laptop');
 INSERT INTO federation_enrollments(token_hash,spoke_instance_uid,project_id,capabilities,bound_actor) SELECT repeat('b',64),'00000000000000000000000003',id,'pull,push','member' FROM projects WHERE name='existing-project';`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "relay_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), version)

	visible, err := upgraded.AccessibleProjectUIDs(ctx, "member")
	require.NoError(t, err)
	require.Equal(t, []string{"00000000000000000000000001"}, visible)
	policy, err := upgraded.ProjectAccessPolicy(ctx, "00000000000000000000000001")
	require.NoError(t, err)
	require.Equal(t, "all", policy.Visibility)
	enrollments, err := upgraded.ListFederationEnrollments(ctx)
	require.NoError(t, err)
	require.Len(t, enrollments, 1)
	require.Equal(t, 0, enrollments[0].RelayProtocolVersion, "an enrollment made before relay stays a legacy grant")
	require.Nil(t, enrollments[0].ParentTokenID)
	require.False(t, enrollments[0].RelayServeDownstream)
	var title string
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT title FROM issues WHERE uid=$1`, "00000000000000000000000002").Scan(&title))
	require.Equal(t, "Existing task", title)
	dbtest.RunEmbeddingArtifactStorage(t, upgraded)

	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "relay_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	require.Equal(t, projectAccessStructure(t, fresh, "relay_fresh"), projectAccessStructure(t, upgraded, "relay_upgrade"), "upgrade and canonical schemas must agree")
}
