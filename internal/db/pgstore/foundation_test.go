package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
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

func TestFirstPostgresReleaseHasNoHistoricalMigrationAssets(t *testing.T) {
	t.Parallel()

	assert.Empty(t, pgstore.Migrations(),
		"Postgres has not shipped yet, so its first canonical schema is the migration floor")
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

	runtimeRole := fmt.Sprintf("example_runtime_%d", os.Getpid())
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		REVOKE CREATE ON SCHEMA isolated_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA isolated_store TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA isolated_store TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA isolated_store TO %s;
`, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)
	runtimeStore, err := pgstore.OpenWithConfig(ctx, postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"), pgstore.Config{
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
		postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"),
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

func TestRestrictedRuntimeRoleCanAdoptExistingProject(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	const schema = "adoption_store"
	owner, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	project, err := owner.CreateProject(ctx, "existing-project")
	require.NoError(t, err)
	require.NoError(t, owner.Close())

	runtimeRole := fmt.Sprintf("adoption_runtime_%d", os.Getpid())
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		REVOKE CREATE ON SCHEMA adoption_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA adoption_store TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA adoption_store TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA adoption_store TO %s;
		GRANT EXECUTE ON FUNCTION adoption_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
	`, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)

	runtime, err := pgstore.OpenWithConfig(ctx,
		postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"),
		pgstore.Config{Schema: schema, SchemaMode: pgstore.SchemaModeValidate},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close() })
	hubProjectUID := "01KATA00000000000000000077"
	conn, err := runtime.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		CREATE TEMP TABLE federation_bindings (project_id BIGINT);
		CREATE TEMP TABLE projects (id BIGINT PRIMARY KEY, uid TEXT, deleted_at TEXT);
		CREATE FUNCTION pg_temp.allow_uid_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RETURN NEW;
		END $$;
		CREATE TRIGGER trg_projects_uid_immutable
			BEFORE UPDATE OF uid ON projects
			FOR EACH ROW EXECUTE FUNCTION pg_temp.allow_uid_update();
	`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`INSERT INTO projects(id, uid) VALUES ($1, '01KATA00000000000000000001')`, project.ID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx,
		`SELECT adoption_store.rewrite_project_uid_for_adoption($1, $2)`, project.ID, hubProjectUID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DISCARD TEMP`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	var adoptedUID string
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT uid FROM adoption_store.projects WHERE id=$1`, project.ID).Scan(&adoptedUID))
	require.Equal(t, hubProjectUID, adoptedUID,
		"temporary relations owned by the runtime role must not shadow adoption targets")

	result, err := runtime.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: project.ID, HubURL: "https://hub.example", HubProjectID: 77,
		HubProjectUID: hubProjectUID, ReplayHorizonEventID: 1, Actor: "adoption-agent",
	})
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, result.Project.UID)

	_, err = runtime.ExecContext(ctx, `ALTER TABLE projects DISABLE TRIGGER trg_projects_uid_immutable`)
	require.Error(t, err, "runtime role must remain unable to alter schema objects")
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

func TestBootstrapRejectsUnshippedOlderPostgresVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	_, err = admin.ExecContext(ctx, `CREATE SCHEMA legacy_store;
CREATE TABLE legacy_store.meta (key text PRIMARY KEY, value text NOT NULL);
INSERT INTO legacy_store.meta(key, value) VALUES ('schema_version', '23')`)
	require.NoError(t, err)

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "legacy_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	assert.Nil(t, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no postgres migration path from schema_version 23")
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

func TestValidatePreparedCanonicalSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	prepared, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "external_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	require.NoError(t, prepared.Close())

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
