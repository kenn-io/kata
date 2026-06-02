package storeopen_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
)

// TestOpen_BarePathBootstrapsFreshSQLite opens a bare filesystem path and
// confirms the returned Storage is a working SQLite backend that accepts a
// real mutation. Open auto-bootstraps the schema in one transaction when the
// file is fresh.
func TestOpen_BarePathBootstrapsFreshSQLite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.CreateProject(ctx, "bare-path-project")
	require.NoError(t, err)
}

// TestOpen_SQLiteSchemeBootstrapsFreshSQLite opens a sqlite://-prefixed DSN
// and confirms the trim leaves a working SQLite backend.
func TestOpen_SQLiteSchemeBootstrapsFreshSQLite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, err := storeopen.Open(ctx, "sqlite://"+path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.CreateProject(ctx, "sqlite-scheme-project")
	require.NoError(t, err)
}

// TestOpen_PostgresSchemeRejectedUntilDomainMethodsLand proves production
// store opening does not route normal callers into pgstore while core domain
// methods are still stubs. The embedded password must not be echoed in the
// error path.
func TestOpen_PostgresSchemeRejectedUntilDomainMethodsLand(t *testing.T) {
	ctx := context.Background()
	rawDSN := "postgres://user:SECRET@127.0.0.1:1/kata?connect_timeout=1&sslmode=disable" //nolint:gosec // fixture

	store, err := storeopen.Open(ctx, rawDSN)
	assert.Nil(t, store)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "not selectable")
	assert.NotContains(t, msg, "SECRET",
		"password must not leak into the rejection message")
}

// TestOpen_UnknownSchemeIsUnsupported refuses any non-sqlite/non-postgres
// scheme.
func TestOpen_UnknownSchemeIsUnsupported(t *testing.T) {
	ctx := context.Background()
	store, err := storeopen.Open(ctx, "mysql://h/db")
	assert.Nil(t, store)
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "unsupported"), "error must mark scheme as unsupported, got %q", msg)
}

// TestOpen_RunsCutoverOnPreCurrentSQLite stands up a DB whose
// schema_version is below db.CurrentSchemaVersion() and confirms storeopen
// runs jsonl.AutoCutover before opening.
func TestOpen_RunsCutoverOnPreCurrentSQLite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Stand up a fixture and rewrite meta.schema_version to a pre-current
	// value so storeopen routes through cutover.
	stageLegacyPreCutoverFixture(t, path, db.CurrentSchemaVersion()-1)

	s, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

func TestOpen_RoutesVersionZeroExistingSQLiteThroughCutover(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageLegacyPreCutoverFixture(t, path, 0)

	s, err := storeopen.Open(ctx, path)
	assert.Nil(t, s)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "table projects already exists")

	ver, peekErr := sqlitestore.PeekSchemaVersion(ctx, path)
	require.NoError(t, peekErr)
	assert.Equal(t, 0, ver, "storeopen must not bootstrap over an existing version-0 DB")
}

// stageLegacyPreCutoverFixture creates a real kata-shaped SQLite DB at path
// and rewrites meta.schema_version to a value below the current version so
// jsonl.AutoCutover treats it as legacy. Open gives us all the tables
// AutoCutover's export step expects without hand-writing a baseline schema.
func stageLegacyPreCutoverFixture(t *testing.T, path string, version int) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`, strconv.Itoa(version))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// TestOpen_RejectsNewerThanBinary confirms that a DB stamped with a
// schema_version above the binary's current is refused with a distinct
// error.
func TestOpen_RejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageNewerThanBinaryFixture(t, path)

	_, err := storeopen.Open(ctx, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
}

// stageNewerThanBinaryFixture creates a kata-shaped DB at path and rewrites
// meta.schema_version to a value above db.CurrentSchemaVersion() — the
// "newer DB written by a newer binary" case.
func stageNewerThanBinaryFixture(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// TestOpenReadOnly_OnCurrentDBSucceeds confirms read-only opens against a
// current-version DB return a usable handle.
func TestOpenReadOnly_OnCurrentDBSucceeds(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Stand up a current-version DB via storeopen (auto-bootstrap).
	s, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen read-only.
	ro, err := storeopen.OpenReadOnly(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	v, err := ro.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

// TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError proves KATA_DSN
// plumbing reaches storeopen.Open end-to-end and that the Postgres rejection
// path never echoes the password embedded in the DSN.
func TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:SECRET@127.0.0.1:1/kata?connect_timeout=1&sslmode=disable") //nolint:gosec // fixture
	t.Setenv("KATA_DB", "")

	dsn, err := config.KataDSN(context.Background())
	require.NoError(t, err)
	require.Contains(t, dsn, "SECRET", "raw DSN keeps the secret; redaction happens at error time")

	_, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not selectable")
	assert.NotContains(t, err.Error(), "SECRET",
		"password must not leak into the rejection message")
}

// TestOpenResolvedFromStorageDSNKeepsPasswordOutOfError mirrors the env case
// but for the [storage].dsn TOML branch. Same connection-fail strategy.
func TestOpenResolvedFromStorageDSNKeepsPasswordOutOfError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	body := "[storage]\ndsn = \"postgres://user:SECRET@127.0.0.1:1/kata?connect_timeout=1&sslmode=disable\"\n" //nolint:gosec // fixture
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))

	dsn, err := config.KataDSN(context.Background())
	require.NoError(t, err)
	require.Contains(t, dsn, "SECRET")

	_, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not selectable")
	assert.NotContains(t, err.Error(), "SECRET")
}

func TestDatabaseOpenTerminologyAvoidsMigrationLanguage(t *testing.T) {
	repoRoot := filepath.Clean("../../..")
	files := []string{
		"cmd/kata/testhelpers_test.go",
		"cmd/kata/export_test.go",
		"cmd/kata/import_test.go",
		"internal/db/pgstore/open.go",
		"internal/db/pgstore/store.go",
		"internal/db/pgstore/stubs_gen.go",
		"internal/db/pgstore/stubgen/main.go",
		"internal/db/sqlitestore/schema_completeness_test.go",
		"internal/db/sqlitestore/store.go",
		"internal/db/storeopen/storeopen.go",
		"internal/jsonl/cutover_test.go",
		"internal/jsonl/testdb_helper_test.go",
	}
	banned := []string{
		"already-migrated",
		"migrate externally",
		"migrate.go",
		"migration runner",
		"openMigrated",
	}
	for _, file := range files {
		body, err := os.ReadFile(filepath.Join(repoRoot, file)) //nolint:gosec // test reads a static allowlist of repository files
		require.NoError(t, err)
		text := string(body)
		for _, phrase := range banned {
			if strings.Contains(text, phrase) {
				t.Errorf("%s still contains stale DB-open terminology %q", file, phrase)
			}
		}
	}
}
