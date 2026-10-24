package pgstore_test

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

//go:embed testdata/schema_v34.sql
var scopedSchema34 string

// Actual schema-34 DDL upgrades through the production migration runner. Fresh
// and upgraded schemas must expose the same objects, indexes and constraints.
func TestScopedSchemaUpgradeAndFreshParity(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA scoped_upgrade; SET LOCAL search_path TO scoped_upgrade, pg_catalog, pg_temp;`+scopedSchema34+`
 INSERT INTO meta(key,value) VALUES('schema_version','34'),('instance_uid','00000000000000000000000009');
 INSERT INTO projects(uid,name) VALUES('00000000000000000000000001','example-project');`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	upgraded, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "scoped_upgrade", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	fresh, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "scoped_fresh", SchemaMode: pgstore.SchemaModeBootstrap})
	require.NoError(t, err)
	t.Cleanup(func() { _ = fresh.Close() })
	var count int
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname='scoped_upgrade' AND (tablename LIKE 'federation_scoped_%' OR tablename LIKE 'federation_source_%' OR tablename IN ('federation_private_sources','federation_control_custody','federation_mutation_expectations','federation_predicate_incarnations','federation_canonical_close_state','federation_protocol_cutovers','federation_conversion_obligations'))`).Scan(&count))
	require.Equal(t, 20, count)
	require.Equal(t, projectAccessStructure(t, fresh, "scoped_fresh"), projectAccessStructure(t, upgraded, "scoped_upgrade"))
	var name string
	require.NoError(t, upgraded.QueryRowContext(ctx, `SELECT name FROM projects WHERE uid='00000000000000000000000001'`).Scan(&name))
	require.Equal(t, "example-project", name)
	version, err := upgraded.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 35, version)
	// Validate-only runtime roles receive the exact same new table privileges in
	// fresh and upgraded schemas; missing a new custody table grant fails startup.
	runtimeRole := fmt.Sprintf("scoped_runtime_%d_%d", os.Getpid(), time.Now().UnixNano())
	var owner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&owner))
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'fixture-runtime-password'`, runtimeRole))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.Background(), fmt.Sprintf(`DROP OWNED BY %s; DROP ROLE %s`, runtimeRole, runtimeRole))
		require.NoError(t, err)
	})
	for _, schema := range []string{"scoped_upgrade", "scoped_fresh"} {
		_, err = admin.ExecContext(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA %s TO %s; GRANT USAGE,SELECT,UPDATE ON ALL SEQUENCES IN SCHEMA %s TO %s; GRANT EXECUTE ON FUNCTION %s.rewrite_project_uid_for_adoption(BIGINT,TEXT) TO %s`, schema, runtimeRole, schema, runtimeRole, schema, runtimeRole, schema, runtimeRole))
		require.NoError(t, err)
		config := pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeValidate, SchemaOwner: owner}
		runtimeDSN := postgresDSNWithUser(t, dsn, runtimeRole, "fixture-runtime-password")
		runtime, err := pgstore.OpenWithConfig(ctx, runtimeDSN, config)
		require.NoError(t, err)
		_, err = runtime.ExecContext(ctx, `CREATE TABLE must_not_exist(id INTEGER)`)
		require.Error(t, err, "runtime remains unable to modify schema")
		require.NoError(t, runtime.Close())
		_, err = admin.ExecContext(ctx, fmt.Sprintf(`REVOKE SELECT ON %s.federation_control_custody FROM %s`, schema, runtimeRole))
		require.NoError(t, err)
		refused, err := pgstore.OpenWithConfig(ctx, runtimeDSN, config)
		require.Nil(t, refused)
		require.ErrorContains(t, err, "federation_control_custody")
	}

}

func TestScopedMigrationStampRollsBackWithDDL(t *testing.T) {
	ctx := t.Context()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA scoped_interrupted; SET LOCAL search_path TO scoped_interrupted,pg_catalog,pg_temp;`+scopedSchema34+`
 INSERT INTO meta(key,value) VALUES('schema_version','34'),('instance_uid','00000000000000000000000009');
 CREATE TABLE federation_scoped_batches(blocker INTEGER);`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	failed, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{Schema: "scoped_interrupted", SchemaMode: pgstore.SchemaModeBootstrap})
	require.Error(t, err)
	require.Nil(t, failed)
	require.Contains(t, err.Error(), "000035_scoped_relay.up.sql")
	var version string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT value FROM scoped_interrupted.meta WHERE key='schema_version'`).Scan(&version))
	require.Equal(t, "34", version)
	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT to_regclass('scoped_interrupted.federation_scoped_views') IS NOT NULL`).Scan(&exists))
	require.False(t, exists, "earlier DDL in the failed migration rolled back with stamp")
}
