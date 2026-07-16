package storeopen_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/testenv"
	_ "modernc.org/sqlite"
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

func TestValidateAcceptsPostgresSchemes(t *testing.T) {
	t.Parallel()

	assert.NoError(t, storeopen.Validate("postgres://db.example/kata"))
	assert.NoError(t, storeopen.Validate("postgresql://db.example/kata"))
}

// TestOpen_PostgresDSNRoundTripsThroughReadOnlyOpen proves production store
// selection routes a Postgres DSN to a usable backend. The read-only reopen
// must observe the committed project and reject a real mutation.
func TestOpen_PostgresDSNRoundTripsThroughReadOnlyOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	ctx := context.Background()
	dsn, cleanup := testenv.NewPostgresContainer(t, ctx)
	t.Cleanup(cleanup)

	store, err := storeopen.Open(ctx, dsn)
	require.NoError(t, err)
	created, err := store.CreateProject(ctx, "selected-backend")
	require.NoError(t, err)
	require.NoError(t, store.Close())

	readOnly, err := storeopen.OpenReadOnly(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = readOnly.Close() })
	got, err := readOnly.ProjectByName(ctx, "selected-backend")
	require.NoError(t, err)
	assert.Equal(t, created.UID, got.UID)
	_, err = readOnly.CreateProject(ctx, "must-not-write")
	assert.Error(t, err)
}

func TestOpenWithConfig_PostgresValidateModeDoesNotInstallSchema(t *testing.T) {
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

	store, err := storeopen.OpenWithConfig(ctx, dsn, storeopen.Config{
		Postgres: pgstore.Config{
			Schema: "runtime_store", SchemaMode: pgstore.SchemaModeValidate,
			SchemaOwner: schemaOwner,
		},
	})
	assert.Nil(t, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "runtime_store" is not installed`)

	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'runtime_store')`).Scan(&exists))
	assert.False(t, exists)
}

func TestOpen_PostgresHonorsResolvedValidationPolicy(t *testing.T) {
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
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	t.Setenv("KATA_POSTGRES_SCHEMA", "resolved_store")
	t.Setenv("KATA_POSTGRES_SCHEMA_MODE", "validate")
	t.Setenv("KATA_POSTGRES_SCHEMA_OWNER", schemaOwner)

	store, err := storeopen.Open(ctx, dsn)
	assert.Nil(t, store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `schema "resolved_store" is not installed`)

	var exists bool
	require.NoError(t, admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'resolved_store')`).Scan(&exists))
	assert.False(t, exists)
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
// plumbing reaches storeopen.Open end-to-end and that a connection failure
// never echoes the password embedded in the DSN.
func TestOpenResolvedFromKataDSNEnvKeepsPasswordOutOfError(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_DSN", "postgres://user:SECRET@127.0.0.1:1/kata?connect_timeout=1&sslmode=disable") //nolint:gosec // fixture
	t.Setenv("KATA_DB", "")

	dsn, err := config.KataDSN(context.Background())
	require.NoError(t, err)
	require.Contains(t, dsn, "SECRET", "raw DSN keeps the secret; redaction happens at error time")

	_, err = storeopen.Open(context.Background(), dsn)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET",
		"password must not leak into the connection error")
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
	assert.NotContains(t, err.Error(), "SECRET")
}

// TestOpen_V14CutoverCarriesLinks proves the v14→v16 storage cutover
// (links.project_id removed, same-project triggers dropped) carries a link
// row intact through the full storeopen.Open path.
//
// Fixture mechanism: the current-schema DB is bootstrapped by sqlitestore.Open
// and then rebuilt in-place to the real v14 physical shape (links table WITH
// project_id and the same-project enforcement trigger). One project with two
// issues and one same-project link are seeded; schema_version is set to 14.
// storeopen.Open sees version 14 < 15, routes through AutoCutover, and the
// assertions below verify the v16 DB looks correct.
//
// The export step (exportLinks, sourceSchemaVersion=14) does NOT include
// project_id in its SELECT, so the JSONL link records produced by the cutover
// do not carry a stray project_id key. Stray-key tolerance is covered
// separately by TestImport_LinkWithStrayProjectIDKeyIsIgnored in
// internal/jsonl/cutover_test.go.
func TestOpen_V14CutoverCarriesLinks(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV14DBWithLink(t, path)

	s, err := storeopen.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// Schema bumped to v16 by cutover.
	v, err := s.SchemaVersion(ctx)
	require.NoError(t, err)
	assert.Equal(t, db.CurrentSchemaVersion(), v)

	// Open a raw connection to assert structural invariants.
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	// links table must NOT have a project_id column.
	cols := tableColumns(t, raw, "links")
	assert.NotContains(t, cols, "project_id", "v16 links table must not have project_id")
	assert.Contains(t, cols, "from_issue_uid", "from_issue_uid must survive")
	assert.Contains(t, cols, "to_issue_uid", "to_issue_uid must survive")

	// The seeded link row must have survived with correct endpoints and type.
	var fromUID, toUID, linkType, author string
	err = raw.QueryRowContext(ctx,
		`SELECT from_issue_uid, to_issue_uid, type, author FROM links`).
		Scan(&fromUID, &toUID, &linkType, &author)
	require.NoError(t, err)
	assert.Equal(t, v14FromIssueUID, fromUID, "from_issue_uid must survive cutover")
	assert.Equal(t, v14ToIssueUID, toUID, "to_issue_uid must survive cutover")
	assert.Equal(t, "blocks", linkType)
	assert.Equal(t, "tester", author)

	// Sanity: both issues must have survived.
	var issueCount int
	require.NoError(t, raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues`).Scan(&issueCount))
	assert.Equal(t, 2, issueCount, "both seeded issues must survive cutover")
}

// Issue UIDs used by seedV14DBWithLink. Fixed values so assertions are
// deterministic; Crockford base32 characters only so short_id derivation
// accepts them.
const (
	v14FromIssueUID = "01HZZZZZZZZZZZZZZZZZZZZA01"
	v14ToIssueUID   = "01HZZZZZZZZZZZZZZZZZZZZA02"
)

// seedV14DBWithLink builds a SQLite database at path whose physical shape
// matches storage v14: links has project_id + same-project enforcement trigger,
// and meta.schema_version = 14. One project with two issues and one same-
// project "blocks" link is seeded.
//
// The fixture mechanism mirrors seedV8DBWithOrphans in internal/jsonl: bootstrap
// the current v16 schema via sqlitestore.Open (so meta, sqlite_sequence, and all
// unrelated tables exist in the right shape), then rebuild links in-place to the
// v14 physical shape, seed rows, and rewrite schema_version.
//
// Deliberate subset: the embedded shape is the minimal v14 subset the cutover
// read path touches. idx_links_project, idx_links_from_uid/to_uid,
// trg_links_same_project_update, and both trg_links_uid_consistency_* triggers
// are intentionally omitted because the export SELECT never exercises them.
func seedV14DBWithLink(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	// Disable FK enforcement while rebuilding the links table to v14 shape.
	_, err = raw.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)

	// Drop v16 links objects (index + triggers + table).
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS uniq_one_parent_per_child`,
		`DROP INDEX IF EXISTS idx_links_from`,
		`DROP INDEX IF EXISTS idx_links_to`,
		`DROP INDEX IF EXISTS idx_links_from_uid`,
		`DROP INDEX IF EXISTS idx_links_to_uid`,
		`DROP TRIGGER IF EXISTS trg_links_uid_consistency_insert`,
		`DROP TRIGGER IF EXISTS trg_links_uid_consistency_update`,
		`DROP TABLE links`,
	} {
		_, err = raw.ExecContext(ctx, stmt)
		require.NoErrorf(t, err, "drop v16 links object: %s", stmt)
	}

	// Recreate links as v14: WITH project_id + same-project trigger.
	_, err = raw.ExecContext(ctx, `
		CREATE TABLE links (
		  id            INTEGER PRIMARY KEY AUTOINCREMENT,
		  project_id    INTEGER NOT NULL REFERENCES projects(id),
		  from_issue_id INTEGER NOT NULL REFERENCES issues(id),
		  to_issue_id   INTEGER NOT NULL REFERENCES issues(id),
		  from_issue_uid TEXT NOT NULL,
		  to_issue_uid   TEXT NOT NULL,
		  type          TEXT NOT NULL CHECK(type IN ('parent','blocks','related')),
		  author        TEXT NOT NULL,
		  created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  UNIQUE(from_issue_id, to_issue_id, type),
		  CHECK (from_issue_id <> to_issue_id),
		  CHECK (length(from_issue_uid) = 26),
		  CHECK (length(to_issue_uid)   = 26),
		  CHECK (length(trim(author)) > 0),
		  CHECK (type <> 'related' OR from_issue_id < to_issue_id)
		)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `
		CREATE TRIGGER trg_links_same_project_insert
		BEFORE INSERT ON links
		FOR EACH ROW BEGIN
		  SELECT RAISE(ABORT, 'cross-project links are not allowed')
		  WHERE (SELECT project_id FROM issues WHERE id = NEW.from_issue_id)
		     <> (SELECT project_id FROM issues WHERE id = NEW.to_issue_id);
		END`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE UNIQUE INDEX uniq_one_parent_per_child ON links(from_issue_id) WHERE type = 'parent'`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE INDEX idx_links_from ON links(from_issue_id, type)`)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `CREATE INDEX idx_links_to ON links(to_issue_id, type)`)
	require.NoError(t, err)

	// Remove the auto-created system project so our seed project can use id=1.
	_, err = raw.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, db.SystemProjectName)
	require.NoError(t, err)

	// Seed one project and two issues.
	const projUID = "01HZZZZZZZZZZZZZZZZZZZZZZZ"
	_, err = raw.ExecContext(ctx,
		`INSERT INTO projects(id, uid, name, metadata, revision) VALUES(1, ?, 'spoke-project', '{}', 1)`,
		projUID)
	require.NoError(t, err)
	for _, row := range []struct {
		id      int64
		uid     string
		shortID string
	}{
		{1, v14FromIssueUID, "za01"},
		{2, v14ToIssueUID, "za02"},
	} {
		_, err = raw.ExecContext(ctx,
			`INSERT INTO issues(id, uid, project_id, short_id, title, author, metadata, revision)
			 VALUES(?, ?, 1, ?, ?, 'tester', '{}', 1)`,
			row.id, row.uid, row.shortID, "issue "+row.shortID)
		require.NoError(t, err)
	}

	// FK enforcement back on so the link INSERT validates correctly.
	_, err = raw.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	_, err = raw.ExecContext(ctx,
		`INSERT INTO links(project_id, from_issue_id, from_issue_uid, to_issue_id, to_issue_uid, type, author)
		 VALUES(1, 1, ?, 2, ?, 'blocks', 'tester')`,
		v14FromIssueUID, v14ToIssueUID)
	require.NoError(t, err)

	_, err = raw.ExecContext(ctx, `UPDATE meta SET value='14' WHERE key='schema_version'`)
	require.NoError(t, err)
}

// tableColumns returns the set of column names for table in raw.
func tableColumns(t *testing.T, raw *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('` + table + `')`) //nolint:gosec // table is a test-controlled literal
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	cols := make(map[string]bool)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		cols[name] = true
	}
	require.NoError(t, rows.Err())
	return cols
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
