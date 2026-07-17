package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestStoragePostgresMigrateAndStatusWithSeparatedRoles(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", dsn)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_POSTGRES_SCHEMA", "operator_store")

	out := executeRoot(t, newRootCmd(), "--json", "storage", "postgres", "migrate")
	var migrated struct {
		Action        string `json:"action"`
		Backend       string `json:"backend"`
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(out, &migrated))
	assert.Equal(t, "migrate", migrated.Action)
	assert.Equal(t, "postgres", migrated.Backend)
	assert.Equal(t, "operator_store", migrated.Schema)
	assert.Equal(t, db.CurrentSchemaVersion(), migrated.SchemaVersion)
	assert.Equal(t, "ready", migrated.Status)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
	t.Setenv("KATA_POSTGRES_SCHEMA_OWNER", schemaOwner)
	runtimeRole := fmt.Sprintf("operator_runtime_%d", os.Getpid())
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		REVOKE CREATE ON SCHEMA operator_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA operator_store TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA operator_store TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA operator_store TO %s;
		GRANT EXECUTE ON FUNCTION operator_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
	`, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)

	runtimeDSN := postgresDSNForCLIUser(t, dsn, runtimeRole, "runtime-password")
	t.Setenv("KATA_DSN", runtimeDSN)
	out = executeRoot(t, newRootCmd(), "--agent", "storage", "postgres", "status")
	assert.Equal(t,
		"OK postgres_schema action=status backend=postgres schema=operator_store schema_version=25 status=ready\n",
		string(out))
	assert.NotContains(t, string(out), "runtime-password")

	_, err = admin.ExecContext(ctx,
		fmt.Sprintf(`SET ROLE %s; CREATE TABLE operator_store.forbidden(id integer)`, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.Error(t, err, "runtime role must not hold schema DDL authority")
}

func TestStoragePostgresRejectsSQLiteTarget(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", t.TempDir()+"/kata.db")
	_, err := runCmdOutput(t, nil, "storage", "postgres", "status")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a postgres DSN")
}

func TestStoragePostgresStatusRejectsIncompleteRuntimePrivileges(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", dsn)
	t.Setenv("KATA_DB", "")
	t.Setenv("KATA_POSTGRES_SCHEMA", "privilege_store")
	_ = executeRoot(t, newRootCmd(), "storage", "postgres", "migrate")

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
	t.Setenv("KATA_POSTGRES_SCHEMA_OWNER", schemaOwner)

	for index, test := range []struct {
		name   string
		revoke string
		want   string
	}{
		{name: "schema usage", revoke: `REVOKE USAGE ON SCHEMA privilege_store FROM %s`, want: `USAGE privilege on schema "privilege_store"`},
		{name: "table insert", revoke: `REVOKE INSERT ON privilege_store.projects FROM %s`, want: `INSERT privilege on table "privilege_store.projects"`},
		{name: "sequence update", revoke: `REVOKE UPDATE ON SEQUENCE privilege_store.projects_id_seq FROM %s`, want: `UPDATE privilege on sequence "privilege_store.projects_id_seq"`},
		{name: "adoption function execute", revoke: `REVOKE EXECUTE ON FUNCTION privilege_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) FROM %s`, want: `EXECUTE privilege on function "privilege_store.rewrite_project_uid_for_adoption(bigint,text)"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeRole := fmt.Sprintf("status_runtime_%d_%d", os.Getpid(), index)
			_, err := admin.ExecContext(ctx, fmt.Sprintf(`
				CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
				GRANT USAGE ON SCHEMA privilege_store TO %s;
				GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA privilege_store TO %s;
				GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA privilege_store TO %s;
				GRANT EXECUTE ON FUNCTION privilege_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
			`, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID and table index.
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`
					DROP OWNED BY %s;
					DROP ROLE IF EXISTS %s;
				`, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID and table index.
			})
			_, err = admin.ExecContext(ctx, fmt.Sprintf(test.revoke, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID and table index.
			require.NoError(t, err)
			t.Setenv("KATA_DSN", postgresDSNForCLIUser(t, dsn, runtimeRole, "runtime-password"))

			_, err = runCmdOutput(t, nil, "storage", "postgres", "status")
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestDaemonPreflightCarriesPostgresValidationPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "postgres://db.example/kata")
	t.Setenv("KATA_DB", "")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
[storage.postgres]
schema = "runtime_store"
mode = "validate"
schema_owner = "runtime_schema_owner"
`), 0o600))

	startup, err := preflightDaemonStartup(context.Background(), "127.0.0.1:0", false)
	require.NoError(t, err)
	assert.Equal(t, "runtime_store", startup.StoreConfig.Postgres.Schema)
	assert.Equal(t, "validate", string(startup.StoreConfig.Postgres.SchemaMode))
	assert.Equal(t, "runtime_schema_owner", startup.StoreConfig.Postgres.SchemaOwner)
}

func postgresDSNForCLIUser(t *testing.T, dsn, username, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(username, password)
	return u.String()
}
