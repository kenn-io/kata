package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/testenv"
)

func TestConfigValidateRejectsUnsafeSchemaNames(t *testing.T) {
	t.Parallel()

	for _, schema := range []string{
		"",
		"Public",
		"public",
		"pg_catalog",
		"pg_shadow",
		"information_schema",
		"two schemas",
		"kata,public",
		`kata"; DROP SCHEMA public CASCADE; --`,
		"0123",
		"schema-with-dash",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		t.Run(schema, func(t *testing.T) {
			t.Parallel()
			err := (pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap}).Validate()
			assert.Error(t, err)
		})
	}

	for _, schema := range []string{"kata", "kata_2", "example_schema"} {
		t.Run(schema, func(t *testing.T) {
			t.Parallel()
			err := (pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap}).Validate()
			assert.NoError(t, err)
		})
	}
}

func TestMigrationAssetsFormCurrentSchemaChain(t *testing.T) {
	t.Parallel()

	migrations := pgstore.Migrations()
	require.Len(t, migrations, 3)
	assert.Equal(t, 0, migrations[0].FromVersion)
	assert.Equal(t, 23, migrations[0].ToVersion)
	assert.Equal(t, "0023_baseline.sql", migrations[0].Name)
	assert.NotEmpty(t, migrations[0].SQL)
	assert.Equal(t, 23, migrations[1].FromVersion)
	assert.Equal(t, 24, migrations[1].ToVersion)
	assert.Equal(t, "0024_postgres_runtime.sql", migrations[1].Name)
	assert.NotEmpty(t, migrations[1].SQL)
	assert.Equal(t, 24, migrations[2].FromVersion)
	assert.Equal(t, db.CurrentSchemaVersion(), migrations[2].ToVersion)
	assert.Equal(t, "0025_federation_binding_authority.sql", migrations[2].Name)
	assert.NotEmpty(t, migrations[2].SQL)
}

func TestOpenUpgradesOriginalVersion24FederationBindings(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`CREATE SCHEMA original_v24`,
		`SET LOCAL search_path TO "original_v24"`,
		pgstore.Migrations()[0].SQL,
		pgstore.Migrations()[1].SQL,
		`ALTER TABLE federation_bindings DROP COLUMN bound_actor, DROP COLUMN allow_insecure`,
		`INSERT INTO projects(uid,name) VALUES ('01HZNQ7VFPK1XGD8R5MABCD4EF','legacy-project')`,
		`INSERT INTO federation_bindings(project_id,role,hub_url,hub_project_id,hub_project_uid,push_enabled)
		 SELECT id,'spoke','https://hub.example',42,uid,1 FROM projects WHERE name='legacy-project'`,
		`INSERT INTO meta(key,value) VALUES ('schema_version','24')
		 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`,
	} {
		_, err = tx.ExecContext(ctx, statement)
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "original_v24", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	version, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	project, err := store.ProjectByName(ctx, "legacy-project")
	require.NoError(t, err)
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Empty(t, binding.Actor)
	assert.False(t, binding.AllowInsecure)
	assert.False(t, binding.PushEnabled)
}

func TestOpenWithConfigIsolatesAndValidatesSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `CREATE TABLE public.meta (key text PRIMARY KEY, value text NOT NULL)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(ctx, `INSERT INTO public.meta(key, value) VALUES ('schema_version', '999')`)
	require.NoError(t, err)

	cfg := pgstore.Config{Schema: "isolated_store", SchemaMode: pgstore.SchemaModeBootstrap}
	store, err := pgstore.OpenWithConfig(ctx, dsn, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	var currentSchema string
	require.NoError(t, store.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&currentSchema))
	assert.Equal(t, "isolated_store", currentSchema)
	var searchPath string
	require.NoError(t, store.QueryRowContext(ctx, `SHOW search_path`).Scan(&searchPath))
	assert.Equal(t, `"isolated_store"`, searchPath)
	version, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)

	var publicVersion string
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT value FROM public.meta WHERE key = 'schema_version'`).Scan(&publicVersion))
	assert.Equal(t, "999", publicVersion)

	_, err = admin.ExecContext(ctx, `
		CREATE ROLE example_runtime LOGIN PASSWORD 'runtime-password';
		REVOKE CREATE ON SCHEMA isolated_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA isolated_store TO example_runtime;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA isolated_store TO example_runtime;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA isolated_store TO example_runtime;
	`)
	require.NoError(t, err)
	runtimeStore, err := pgstore.OpenWithConfig(ctx, postgresDSNWithUser(t, dsn, "example_runtime", "runtime-password"), pgstore.Config{
		Schema:     "isolated_store",
		SchemaMode: pgstore.SchemaModeValidate,
	})
	require.NoError(t, err)
	_, err = runtimeStore.ExecContext(ctx, `CREATE TABLE must_not_be_created (id integer)`)
	require.Error(t, err)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	assert.Equal(t, "42501", pgErr.Code)
	require.NoError(t, runtimeStore.Close())
	readOnlyStore, err := pgstore.OpenWithConfig(
		ctx,
		postgresDSNWithUser(t, dsn, "example_runtime", "runtime-password"),
		pgstore.Config{Schema: "isolated_store", SchemaMode: pgstore.SchemaModeValidate},
		db.ReadOnly(),
	)
	require.NoError(t, err)
	assert.Equal(t, store.InstanceUID(), readOnlyStore.InstanceUID())
	_, err = readOnlyStore.ExecContext(ctx, `DELETE FROM projects`)
	require.Error(t, err)
	require.NoError(t, readOnlyStore.Close())

	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:     "missing_store",
		SchemaMode: pgstore.SchemaModeValidate,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "missing_store" is not installed`)
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:     "missing_store",
		SchemaMode: pgstore.SchemaModeValidate,
	}, db.ReadOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "missing_store" is not installed`)
	var missingExists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'missing_store')`).Scan(&missingExists))
	assert.False(t, missingExists, "validate-only open must not create the configured schema")

	_, err = admin.ExecContext(ctx, `
		CREATE SCHEMA stale_store;
		CREATE TABLE stale_store.meta (key text PRIMARY KEY, value text NOT NULL);
		INSERT INTO stale_store.meta(key, value) VALUES ('schema_version', '999');
	`)
	require.NoError(t, err)
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:     "stale_store",
		SchemaMode: pgstore.SchemaModeValidate,
	}, db.ReadOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_version 999 does not match")
}

func TestBootstrapMultipleSchemasSharesUnaccent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	for _, schema := range []string{"first_store", "second_store"} {
		store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
			Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
		})
		require.NoError(t, err)
		project, err := store.CreateProject(ctx, schema+"-project")
		require.NoError(t, err)
		_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: "résumé search", Author: "conformance-agent",
		})
		require.NoError(t, err, "search triggers must resolve the shared unaccent dictionary")
		require.NoError(t, store.Close())
	}
}

func TestBootstrapUpgradesLegacyVersion23Store(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	_, err = admin.ExecContext(ctx, `CREATE SCHEMA legacy_extensions;
CREATE EXTENSION unaccent WITH SCHEMA legacy_extensions;
CREATE SCHEMA legacy_store`)
	require.NoError(t, err)
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO legacy_store, legacy_extensions`)
	require.NoError(t, err)
	legacySQL := pgstore.Migrations()[0].SQL
	legacySQL = strings.ReplaceAll(legacySQL, "CREATE EXTENSION IF NOT EXISTS unaccent WITH SCHEMA public", "CREATE EXTENSION IF NOT EXISTS unaccent")
	legacySQL = strings.ReplaceAll(legacySQL, `DO $$
BEGIN
  IF (SELECT extnamespace FROM pg_extension WHERE extname = 'unaccent')
       <> (SELECT oid FROM pg_namespace WHERE nspname = 'public') THEN
    ALTER EXTENSION unaccent SET SCHEMA public;
  END IF;
END
$$;
`, "")
	legacySQL = strings.ReplaceAll(legacySQL, "WITH public.unaccent, pg_catalog.simple", "WITH unaccent, simple")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_unique_edge ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_type_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_not_self_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_from_uid_length_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_to_uid_length_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_author_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT links_related_order_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT issue_labels_label_length_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT issue_labels_label_charset_check ", "")
	legacySQL = strings.ReplaceAll(legacySQL, "CONSTRAINT issue_labels_author_check ", "")
	_, err = tx.ExecContext(ctx, legacySQL)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_version', '23')`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "legacy_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	version, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
	var extensionSchema string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT n.nspname
FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname = 'unaccent'`).Scan(&extensionSchema))
	assert.Equal(t, "public", extensionSchema)
	_, err = store.SystemProject(ctx)
	require.NoError(t, err)

	project, err := store.CreateProject(ctx, "legacy-project")
	require.NoError(t, err)
	parentA, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "parent a", Author: "tester"})
	require.NoError(t, err)
	parentB, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "parent b", Author: "tester"})
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "two parents", Author: "tester",
		Links: []db.InitialLink{{Type: "parent", ToNumber: parentA.ID}, {Type: "parent", ToNumber: parentB.ID}},
	})
	assert.ErrorIs(t, err, db.ErrParentAlreadySet)
	_, err = store.AddLabel(ctx, parentA.ID, "Bad Label", "tester")
	assert.ErrorIs(t, err, db.ErrLabelInvalid)
	link, err := store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: parentA.ID, ToIssueID: parentB.ID, Type: "related", Author: "tester",
	})
	require.NoError(t, err)
	assert.NotZero(t, link.ID)
	_, err = store.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: parentA.ID, ToIssueID: parentB.ID, Type: "related", Author: "tester",
	})
	assert.ErrorIs(t, err, db.ErrLinkExists)
	searchIssue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "résumé search", Author: "tester",
	})
	require.NoError(t, err)
	matches, err := store.SearchFTS(ctx, project.ID, "resume", 20, false)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, searchIssue.ID, matches[0].Issue.ID)
}

func TestReadOnlyOpenRequiresSystemProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "missing_system_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	_, err = store.ExecContext(ctx, `DELETE FROM projects WHERE name = $1`, db.SystemProjectName)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "missing_system_store", SchemaMode: pgstore.SchemaModeValidate,
	}, db.ReadOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate system project")
}

func TestSystemProjectCannotBeRenamed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.Open(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	systemProject, err := store.SystemProject(ctx)
	require.NoError(t, err)
	_, err = store.RenameProject(ctx, systemProject.ID, "visible-system-row")
	assert.ErrorIs(t, err, db.ErrNotFound)
	var name string
	require.NoError(t, store.QueryRowContext(ctx,
		`SELECT name FROM projects WHERE id = $1`, systemProject.ID,
	).Scan(&name))
	assert.Equal(t, db.SystemProjectName, name)
}

func postgresDSNWithUser(t *testing.T, dsn, username, password string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(username, password)
	return u.String()
}

func TestConcurrentBootstrapSerializesOneSchemaInstallation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	const workers = 6
	start := make(chan struct{})
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for i := range workers {
		go func() {
			ready.Done()
			<-start
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema:     "concurrent_store",
				SchemaMode: pgstore.SchemaModeBootstrap,
			})
			if err == nil {
				err = store.Close()
			}
			if err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
				return
			}
			errs <- nil
		}()
	}
	ready.Wait()
	close(start)
	for range workers {
		assert.NoError(t, <-errs)
	}

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var version string
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT value FROM concurrent_store.meta WHERE key = 'schema_version'`).Scan(&version))
	assert.Equal(t, fmt.Sprintf("%d", db.CurrentSchemaVersion()), version)
	var versionRows int
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM concurrent_store.meta WHERE key = 'schema_version'`).Scan(&versionRows))
	assert.Equal(t, 1, versionRows)
}

func TestValidateExternallyMigratedSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	tx, err := admin.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `CREATE SCHEMA external_store`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL search_path TO "external_store"`)
	require.NoError(t, err)
	for _, migration := range pgstore.Migrations() {
		_, err = tx.ExecContext(ctx, migration.SQL)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO meta(key, value) VALUES ('schema_version', $1)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, fmt.Sprintf("%d", migration.ToVersion))
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit())

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:     "external_store",
		SchemaMode: pgstore.SchemaModeValidate,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	version, err := store.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), version)
}
