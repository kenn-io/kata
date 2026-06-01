package storeopen_test

import (
	"context"
	"errors"
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

// TestOpen_BarePathWithApplyMigrationsRoutesToSQLite opens a bare filesystem
// path and confirms the returned Storage is a working SQLite backend that
// accepts a real mutation.
func TestOpen_BarePathWithApplyMigrationsRoutesToSQLite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.CreateProject(ctx, "bare-path-project")
	require.NoError(t, err)
}

// TestOpen_SQLiteSchemeWithApplyMigrationsRoutesToSQLite opens a
// sqlite://-prefixed DSN and confirms the trim leaves a working SQLite
// backend.
func TestOpen_SQLiteSchemeWithApplyMigrationsRoutesToSQLite(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	store, _, err := storeopen.Open(ctx, "sqlite://"+path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.CreateProject(ctx, "sqlite-scheme-project")
	require.NoError(t, err)
}

// TestOpen_PostgresSchemeDispatchesToPgstore proves the dispatcher reaches
// pgstore (which then surfaces a real connection error against an
// unreachable host), and that the embedded password is not echoed back in
// the error path. The unreachable host (TEST-NET-1 RFC 5737) guarantees the
// connection attempt fails fast without a real PG.
func TestOpen_PostgresSchemeDispatchesToPgstore(t *testing.T) {
	ctx := context.Background()
	// 192.0.2.1 is reserved for documentation (TEST-NET-1) — guaranteed
	// non-routable so the pgstore.Open ping fails locally.
	rawDSN := "postgres://user:SECRET@192.0.2.1:5432/kata?connect_timeout=1&sslmode=disable" //nolint:gosec // fixture

	store, _, err := storeopen.Open(ctx, rawDSN)
	assert.Nil(t, store)
	require.Error(t, err)
	msg := err.Error()
	assert.NotContains(t, msg, "not yet available",
		"postgres backend is wired in; the deferred message must be gone")
	assert.NotContains(t, msg, "SECRET",
		"password must not leak into the connection-error message")
}

// TestOpen_UnknownSchemeIsUnsupported refuses any non-sqlite/non-postgres
// scheme.
func TestOpen_UnknownSchemeIsUnsupported(t *testing.T) {
	ctx := context.Background()
	store, _, err := storeopen.Open(ctx, "mysql://h/db")
	assert.Nil(t, store)
	require.Error(t, err)
	msg := err.Error()
	assert.True(t, strings.Contains(msg, "unsupported"), "error must mark scheme as unsupported, got %q", msg)
}

// TestOpen_WithoutApplyMigrationsReturnsErrSchemaOutOfDateForMissingDB
// confirms that the bare Open call (no ApplyMigrations) refuses to create a
// fresh database and steers the caller at `kata migrate`.
func TestOpen_WithoutApplyMigrationsReturnsErrSchemaOutOfDateForMissingDB(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	missing := filepath.Join(t.TempDir(), "kata.db")
	_, _, err := storeopen.Open(context.Background(), missing)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSchemaOutOfDate), err)
	assert.Contains(t, err.Error(), "kata migrate")
}

// TestOpen_WithApplyMigrationsCreatesAndMigratesFreshDB confirms a fresh
// database is created and brought to current.
func TestOpen_WithApplyMigrationsCreatesAndMigratesFreshDB(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")

	s, _, err := storeopen.Open(context.Background(), path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.CreateProject(context.Background(), "kata")
	require.NoError(t, err)
}

// TestOpen_WithApplyMigrationsRunsCutoverThenMigrate stands up a DB whose
// schema_version is below the cutover floor and confirms storeopen runs
// jsonl.AutoCutover before opening.
func TestOpen_WithApplyMigrationsRunsCutoverThenMigrate(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Stand up a pre-cutover-threshold fixture. We restamp meta.schema_version
	// to a value below db.BaselineSchemaVersion so storeopen routes through
	// cutover even though the schema is at v12.
	stageLegacyPreCutoverFixture(t, path, db.BaselineSchemaVersion-1)

	s, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)
}

// stageLegacyPreCutoverFixture creates a real kata-shaped SQLite DB at path
// and rewrites meta.schema_version to a value below the cutover floor so
// jsonl.AutoCutover treats it as legacy. Open+Migrate gives us all the tables
// AutoCutover's export step expects without hand-writing a baseline schema.
func stageLegacyPreCutoverFixture(t *testing.T, path string, version int) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate legacy fixture: %v", err)
	}
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`, strconv.Itoa(version))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// TestOpen_WithApplyMigrationsRejectsNewerThanBinary confirms that a DB
// stamped with a schema_version above the binary's current is refused with a
// distinct error (not ErrSchemaOutOfDate / "kata migrate").
func TestOpen_WithApplyMigrationsRejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageNewerThanBinaryFixture(t, path)

	_, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
	assert.NotContains(t, err.Error(), "kata migrate")
}

// TestOpen_WithoutApplyMigrationsRejectsNewerThanBinary mirrors the above but
// without ApplyMigrations.
func TestOpen_WithoutApplyMigrationsRejectsNewerThanBinary(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	stageNewerThanBinaryFixture(t, path)

	_, _, err := storeopen.Open(ctx, path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than binary schema")
	assert.NotContains(t, err.Error(), "kata migrate")
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
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate newer-than-binary fixture: %v", err)
	}
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value=? WHERE key='schema_version'`,
		strconv.Itoa(db.CurrentSchemaVersion()+1))
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

// TestOpenReadOnly_OnStaleReturnsErrSchemaOutOfDate confirms read-only opens
// also surface the "stale" sentinel.
func TestOpenReadOnly_OnStaleReturnsErrSchemaOutOfDate(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")
	stageLegacyPreCutoverFixture(t, path, db.BaselineSchemaVersion-1)

	_, _, err := storeopen.OpenReadOnly(context.Background(), path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrSchemaOutOfDate), err)
}

// TestOpenReadOnly_OnCurrentDropsApplyMigrationsSilently confirms read-only +
// ApplyMigrations is a no-op (option silently dropped, no migration runs).
func TestOpenReadOnly_OnCurrentDropsApplyMigrationsSilently(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	// Stand up a current-version DB via storeopen + ApplyMigrations.
	s, _, err := storeopen.Open(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	require.NoError(t, s.Close())

	// Reopen read-only with ApplyMigrations — the option is silently dropped.
	ro, result, err := storeopen.OpenReadOnly(ctx, path, db.ApplyMigrations())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ro.Close() })
	assert.Equal(t, db.MigrationResult{}, result)
}

// TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError proves the Phase 8
// KATA_DSN plumbing reaches storeopen.Open end-to-end and that pgstore's
// connection-error path never echoes the password embedded in the DSN. The
// DSN points at TEST-NET-1 (192.0.2.0/24) so the open fails fast without a
// real PG.
func TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:SECRET@192.0.2.1:5432/kata?connect_timeout=1&sslmode=disable") //nolint:gosec // fixture
	t.Setenv("KATA_DB", "")

	dsn, err := config.KataDSN(context.Background())
	require.NoError(t, err)
	require.Contains(t, dsn, "SECRET", "raw DSN keeps the secret; redaction happens at error time")

	_, _, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not yet available",
		"postgres backend is wired in; the deferred message must be gone")
	assert.NotContains(t, err.Error(), "SECRET",
		"password must not leak into the connection-error message")
}

// TestOpenResolvedFromStorageDSNKeepsPasswordOutOfError mirrors the env case
// but for the [storage].dsn TOML branch. Same connection-fail strategy.
func TestOpenResolvedFromStorageDSNKeepsPasswordOutOfError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_DSN", "")
	t.Setenv("KATA_DB", "")
	body := "[storage]\ndsn = \"postgres://user:SECRET@192.0.2.1:5432/kata?connect_timeout=1&sslmode=disable\"\n" //nolint:gosec // fixture
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600))

	dsn, err := config.KataDSN(context.Background())
	require.NoError(t, err)
	require.Contains(t, dsn, "SECRET")

	_, _, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not yet available")
	assert.NotContains(t, err.Error(), "SECRET")
}
