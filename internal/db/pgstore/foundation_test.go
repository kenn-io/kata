package pgstore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
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

func TestConfigValidatePostgresIdentifiers(t *testing.T) {
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

	for _, owner := range []string{"MixedCase", "role-with-dash", "two roles", "1owner", " role "} {
		t.Run("owner/"+owner, func(t *testing.T) {
			t.Parallel()
			err := (pgstore.Config{
				Schema: "kata", SchemaMode: pgstore.SchemaModeValidate, SchemaOwner: owner,
			}).Validate()
			assert.NoError(t, err)
		})
	}

	err := (pgstore.Config{
		Schema: "kata", SchemaMode: pgstore.SchemaModeValidate,
		SchemaOwner: strings.Repeat("a", 64),
	}).Validate()
	assert.ErrorContains(t, err, "exceeds 63 bytes")

	cfg := pgstore.ConfigFromValues("kata", "validate", " Exact Role ", false)
	assert.Equal(t, " Exact Role ", cfg.SchemaOwner)
}

func TestValidationModeRequiresConfiguredSchemaOwnerBeforeConnecting(t *testing.T) {
	t.Parallel()

	store, err := pgstore.OpenWithConfig(context.Background(), "not-a-postgres-dsn", pgstore.Config{
		Schema: "kata", SchemaMode: pgstore.SchemaModeValidate,
	})
	assert.Nil(t, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "postgres schema owner is required in validation mode")
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
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
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
		GRANT EXECUTE ON FUNCTION isolated_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
`, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role is a fixed prefix plus process ID.
	require.NoError(t, err)
	runtimeStore, err := pgstore.OpenWithConfig(ctx, postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"), pgstore.Config{
		Schema:      "isolated_store",
		SchemaMode:  pgstore.SchemaModeValidate,
		SchemaOwner: schemaOwner,
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
		pgstore.Config{
			Schema: "isolated_store", SchemaMode: pgstore.SchemaModeValidate,
			SchemaOwner: schemaOwner,
		},
		db.ReadOnly(),
	)
	require.NoError(t, err)
	assert.Equal(t, store.InstanceUID(), readOnlyStore.InstanceUID())
	_, err = readOnlyStore.ExecContext(ctx, `DELETE FROM projects`)
	require.Error(t, err)
	require.NoError(t, readOnlyStore.Close())

	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:      "missing_store",
		SchemaMode:  pgstore.SchemaModeValidate,
		SchemaOwner: schemaOwner,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "missing_store" is not installed`)
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:      "missing_store",
		SchemaMode:  pgstore.SchemaModeValidate,
		SchemaOwner: schemaOwner,
	}, db.ReadOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "missing_store" is not installed`)
	var missingExists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'missing_store')`).Scan(&missingExists))
	assert.False(t, missingExists, "validate-only open must not create the configured schema")

	stale, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "stale_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	require.NoError(t, stale.Close())
	_, err = admin.ExecContext(ctx,
		`UPDATE stale_store.meta SET value='999' WHERE key='schema_version'`)
	require.NoError(t, err)
	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema:      "stale_store",
		SchemaMode:  pgstore.SchemaModeValidate,
		SchemaOwner: schemaOwner,
	}, db.ReadOnly())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema_version 999 does not match")
}

func TestValidationRejectsUnexpectedSchemaAndObjectOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	var trustedOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&trustedOwner))
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "ownership_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	require.NoError(t, store.Close())

	suffix := fmt.Sprintf("%d", os.Getpid())
	attackerRole := "schema_attacker_" + suffix
	runtimeRole := "schema_runtime_" + suffix
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE ROLE %s;
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		REVOKE CREATE ON SCHEMA ownership_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA ownership_store TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ownership_store TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA ownership_store TO %s;
		GRANT EXECUTE ON FUNCTION ownership_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
	`, attackerRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role names are fixed prefixes plus process ID.
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`
			DROP OWNED BY %s, %s CASCADE;
			DROP ROLE IF EXISTS %s, %s;
		`, attackerRole, runtimeRole, attackerRole, runtimeRole)) // #nosec G201 -- role names are fixed prefixes plus process ID.
	})

	assertRejected := func(t *testing.T, want string) {
		t.Helper()
		untrusted, openErr := pgstore.OpenWithConfig(ctx,
			postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"),
			pgstore.Config{
				Schema: "ownership_store", SchemaMode: pgstore.SchemaModeValidate,
				SchemaOwner: trustedOwner,
			},
		)
		if untrusted != nil {
			t.Cleanup(func() { _ = untrusted.Close() })
		}
		require.Error(t, openErr)
		assert.Contains(t, openErr.Error(), want)
	}

	t.Run("namespace owner", func(t *testing.T) {
		_, err := admin.ExecContext(ctx, `ALTER SCHEMA ownership_store OWNER TO `+attackerRole) // #nosec G202 -- role is a fixed prefix plus process ID.
		require.NoError(t, err)
		assertRejected(t, `schema "ownership_store" owner`)
		_, err = admin.ExecContext(ctx, `ALTER SCHEMA ownership_store OWNER TO `+trustedOwner) // #nosec G202 -- owner came from PostgreSQL current_user.
		require.NoError(t, err)
	})

	t.Run("metadata owner", func(t *testing.T) {
		_, err := admin.ExecContext(ctx, `ALTER TABLE ownership_store.meta OWNER TO `+attackerRole) // #nosec G202 -- role is a fixed prefix plus process ID.
		require.NoError(t, err)
		assertRejected(t, `relation "meta"`)
		_, err = admin.ExecContext(ctx, `ALTER TABLE ownership_store.meta OWNER TO `+trustedOwner) // #nosec G202 -- owner came from PostgreSQL current_user.
		require.NoError(t, err)
	})

	t.Run("row security", func(t *testing.T) {
		_, err := admin.ExecContext(ctx, `ALTER TABLE ownership_store.projects ENABLE ROW LEVEL SECURITY`)
		require.NoError(t, err)
		assertRejected(t, `relation "projects"`)
		_, err = admin.ExecContext(ctx, `ALTER TABLE ownership_store.projects DISABLE ROW LEVEL SECURITY`)
		require.NoError(t, err)
	})

	t.Run("schema create privilege", func(t *testing.T) {
		_, err := admin.ExecContext(ctx, `GRANT CREATE ON SCHEMA ownership_store TO `+attackerRole) // #nosec G202 -- role is a fixed prefix plus process ID.
		require.NoError(t, err)
		assertRejected(t, `grants CREATE privilege to non-owner`)
		ownerStore, openErr := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
			Schema: "ownership_store", SchemaMode: pgstore.SchemaModeBootstrap,
		})
		if ownerStore != nil {
			t.Cleanup(func() { _ = ownerStore.Close() })
		}
		require.Error(t, openErr)
		assert.Contains(t, openErr.Error(), `grants CREATE privilege to non-owner`)
		_, err = admin.ExecContext(ctx, `REVOKE CREATE ON SCHEMA ownership_store FROM `+attackerRole) // #nosec G202 -- role is a fixed prefix plus process ID.
		require.NoError(t, err)
	})
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
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
	project, err := owner.CreateProject(ctx, "existing-project")
	require.NoError(t, err)
	require.NoError(t, owner.Close())

	runtimeRole := fmt.Sprintf("adoption_runtime_%d", os.Getpid())
	attackerRole := fmt.Sprintf("adoption_attacker_%d", os.Getpid())
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		CREATE ROLE %s LOGIN PASSWORD 'runtime-password';
		CREATE ROLE %s;
		REVOKE CREATE ON SCHEMA adoption_store FROM PUBLIC;
		GRANT USAGE ON SCHEMA adoption_store TO %s;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA adoption_store TO %s;
		GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA adoption_store TO %s;
		GRANT EXECUTE ON FUNCTION adoption_store.rewrite_project_uid_for_adoption(BIGINT, TEXT) TO %s;
	`, runtimeRole, attackerRole, runtimeRole, runtimeRole, runtimeRole, runtimeRole)) // #nosec G201 -- role names are fixed prefixes plus process ID.
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`
			DROP OWNED BY %s, %s CASCADE;
			DROP ROLE IF EXISTS %s, %s;
		`, attackerRole, runtimeRole, attackerRole, runtimeRole)) // #nosec G201 -- role names are fixed prefixes plus process ID.
	})

	runtime, err := pgstore.OpenWithConfig(ctx,
		postgresDSNWithUser(t, dsn, runtimeRole, "runtime-password"),
		pgstore.Config{
			Schema: schema, SchemaMode: pgstore.SchemaModeValidate, SchemaOwner: schemaOwner,
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = runtime.Close() })
	_, err = admin.ExecContext(ctx, fmt.Sprintf(`
		GRANT USAGE, CREATE ON SCHEMA adoption_store TO %s;
		SET ROLE %s;
		CREATE FUNCTION adoption_store.fail_bigint_eq(BIGINT, BIGINT)
		RETURNS BOOLEAN LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'operator shadow executed';
		END $$;
		CREATE OPERATOR adoption_store.= (
			LEFTARG = BIGINT,
			RIGHTARG = BIGINT,
			FUNCTION = adoption_store.fail_bigint_eq
		);
		RESET ROLE;
		REVOKE CREATE ON SCHEMA adoption_store FROM %s;
	`, attackerRole, attackerRole, attackerRole)) // #nosec G201 -- role name is a fixed prefix plus process ID.
	require.NoError(t, err)
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

func TestBootstrapRejectsPreexistingSchemaObjectsBeforeExecutingThem(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	_, err = admin.ExecContext(ctx, `
		CREATE SCHEMA shadow_store;
		CREATE FUNCTION shadow_store.to_regtype(text) RETURNS regtype
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'preexisting shadow function executed';
		END
		$$;
		CREATE VIEW shadow_store.meta AS
		SELECT 'schema_version'::text AS key,
		       shadow_store.to_regtype('text')::text AS value;
	`)
	require.NoError(t, err)

	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "shadow_store", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	assert.Nil(t, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `postgres schema "shadow_store" exists without migration metadata`)
	assert.NotContains(t, err.Error(), "preexisting shadow function executed")

	var functionStillExists bool
	err = admin.QueryRowContext(ctx, `
		SELECT to_regprocedure('shadow_store.to_regtype(text)') IS NOT NULL
	`).Scan(&functionStillExists)
	require.NoError(t, err)
	assert.True(t, functionStillExists, "rejected bootstrap must not alter pre-existing objects")
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
	var schemaOwner string
	require.NoError(t, store.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
	_, err = store.ExecContext(ctx, `DELETE FROM projects WHERE name = $1`, db.SystemProjectName)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	_, err = pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "missing_system_store", SchemaMode: pgstore.SchemaModeValidate,
		SchemaOwner: schemaOwner,
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

	for _, schema := range []string{"external_store", "a"} {
		t.Run(schema, func(t *testing.T) {
			prepared, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
			})
			require.NoError(t, err)
			var schemaOwner string
			require.NoError(t, prepared.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))
			require.NoError(t, prepared.Close())

			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema:      schema,
				SchemaMode:  pgstore.SchemaModeValidate,
				SchemaOwner: schemaOwner,
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			version, err := store.SchemaVersion(ctx)
			require.NoError(t, err)
			assert.Equal(t, db.CurrentSchemaVersion(), version)
		})
	}
}

func TestValidationRejectsIncompleteCanonicalSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	var schemaOwner string
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT current_user`).Scan(&schemaOwner))

	for _, test := range []struct {
		name     string
		mutation string
		want     string
	}{
		{name: "column", mutation: `ALTER TABLE %s.events DROP COLUMN content_hash CASCADE`, want: `column "events.content_hash"`},
		{name: "column_definition", mutation: `ALTER TABLE %s.events ALTER COLUMN content_hash DROP NOT NULL`, want: `column definitions do not match`},
		{name: "constraint", mutation: `ALTER TABLE %s.links DROP CONSTRAINT links_type_check`, want: `constraint definitions do not match`},
		{name: "constraint_definition", mutation: `ALTER TABLE %s.links DROP CONSTRAINT links_type_check; ALTER TABLE %s.links ADD CONSTRAINT links_type_check CHECK (true)`, want: `constraint definitions do not match`},
		{name: "index", mutation: `DROP INDEX %s.idx_events_idempotency`, want: `index "idx_events_idempotency"`},
		{name: "index_definition", mutation: `DROP INDEX %s.idx_events_idempotency; CREATE INDEX idx_events_idempotency ON %s.events(actor)`, want: `index definitions do not match`},
		{name: "trigger", mutation: `DROP TRIGGER issues_search_after_issue_update ON %s.issues`, want: `trigger "issues_search_after_issue_update"`},
		{name: "trigger_definition", mutation: `ALTER TABLE %s.issues DISABLE TRIGGER issues_search_after_issue_update`, want: `trigger definitions do not match`},
		{name: "function", mutation: `DROP FUNCTION %s.rebuild_issue_search(BIGINT) CASCADE`, want: `function "rebuild_issue_search(bigint)"`},
		{name: "function_definition", mutation: `CREATE OR REPLACE FUNCTION %s.enforce_uid_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$`, want: `function definitions do not match`},
		{name: "function_attribute", mutation: `ALTER FUNCTION %s.rewrite_project_uid_for_adoption(BIGINT, TEXT) SECURITY INVOKER`, want: `function definitions do not match`},
		{name: "text_search", mutation: `DROP TEXT SEARCH CONFIGURATION %s.kata_simple_unaccent`, want: `text search definitions do not match`},
		{name: "text_search_definition", mutation: `ALTER TEXT SEARCH CONFIGURATION %s.kata_simple_unaccent ALTER MAPPING FOR word WITH pg_catalog.simple`, want: `text search definitions do not match`},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema := "manifest_" + test.name
			store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema: schema, SchemaMode: pgstore.SchemaModeBootstrap,
			})
			require.NoError(t, err)
			require.NoError(t, store.Close())
			mutation := strings.ReplaceAll(test.mutation, "%s", schema)
			_, err = admin.ExecContext(ctx, mutation) // #nosec G201 -- schema is a fixed test identifier.
			require.NoError(t, err)

			validated, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
				Schema: schema, SchemaMode: pgstore.SchemaModeValidate, SchemaOwner: schemaOwner,
			})
			if validated != nil {
				t.Cleanup(func() { _ = validated.Close() })
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestBootstrapRejectsIncompleteCurrentSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)
	store, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "bootstrap_manifest", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	_, err = admin.ExecContext(ctx, `DROP INDEX bootstrap_manifest.idx_events_idempotency`)
	require.NoError(t, err)

	reopened, err := pgstore.OpenWithConfig(ctx, dsn, pgstore.Config{
		Schema: "bootstrap_manifest", SchemaMode: pgstore.SchemaModeBootstrap,
	})
	if reopened != nil {
		t.Cleanup(func() { _ = reopened.Close() })
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), `index "idx_events_idempotency"`)
}
