// Internal tests for exportForCutover on a real v9-shape source database.
// The public Export is current-schema-only; these tests reach the unexported
// legacy projection through exportForCutover and therefore live in
// package jsonl. Mirror of the v9 tests that lived in cutover_test.go before
// the backend-neutral Export refactor.
package jsonl

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// assertTableShapeInternal pins a fixture's table to an exact column set so
// a future schema bump that adds a column slips through this internal-package
// fixture's drop list with a loud failure rather than at real-source cutover
// time. Mirror of assertTableShape in fixtures_test.go (which is in
// package jsonl_test and unreachable from here).
func assertTableShapeInternal(t *testing.T, raw *sql.DB, table string, expected []string) {
	t.Helper()
	rows, err := raw.Query(`SELECT name FROM pragma_table_info('` + table + `')`) //nolint:gosec // table is a test-controlled literal
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got = append(got, name)
	}
	require.NoError(t, rows.Err())
	sort.Strings(got)
	want := append([]string(nil), expected...)
	sort.Strings(want)
	assert.Equal(t, want, got,
		"%s columns drifted from the expected v-N shape — if a recent schema bump "+
			"added a column, also drop it from this fixture", table)
}

// dropV10AdditionsInternal trims a freshly-bootstrapped current-schema DB
// back to the v8/v9 column shape. Mirror of dropV10Additions in
// fixtures_test.go (package jsonl_test, unreachable from here).
func dropV10AdditionsInternal(t *testing.T, raw *sql.DB) {
	t.Helper()
	stmts := []string{
		`DROP TABLE IF EXISTS federation_bindings`,
		`DROP TABLE comments`,
		`CREATE TABLE comments (
		  id         INTEGER PRIMARY KEY AUTOINCREMENT,
		  issue_id   INTEGER NOT NULL REFERENCES issues(id),
		  author     TEXT NOT NULL,
		  body       TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  CHECK (length(trim(author)) > 0),
		  CHECK (length(trim(body))   > 0)
		)`,
		`CREATE INDEX idx_comments_issue ON comments(issue_id, created_at)`,
		`DROP TABLE events`,
		`CREATE TABLE events (
		  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		  uid                 TEXT NOT NULL UNIQUE,
		  origin_instance_uid TEXT NOT NULL,
		  project_id          INTEGER NOT NULL REFERENCES projects(id),
		  project_name        TEXT NOT NULL,
		  issue_id            INTEGER REFERENCES issues(id),
		  issue_uid           TEXT,
		  related_issue_id    INTEGER REFERENCES issues(id),
		  related_issue_uid   TEXT,
		  type                TEXT NOT NULL,
		  actor               TEXT NOT NULL,
		  payload             TEXT NOT NULL DEFAULT '{}',
		  created_at          DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  CHECK (length(trim(actor)) > 0),
		  CHECK (json_valid(payload)),
		  CHECK (length(uid) = 26),
		  CHECK (length(origin_instance_uid) = 26)
		)`,
		`CREATE INDEX idx_events_project ON events(project_id, id)`,
		`CREATE INDEX idx_events_issue ON events(issue_id, id) WHERE issue_id IS NOT NULL`,
		`CREATE INDEX idx_events_related ON events(related_issue_id, id) WHERE related_issue_id IS NOT NULL`,
		`CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL`,
		`CREATE INDEX idx_events_related_issue_uid ON events(related_issue_uid) WHERE related_issue_uid IS NOT NULL`,
		`CREATE INDEX idx_events_origin_instance ON events(origin_instance_uid)`,
		`DROP INDEX IF EXISTS issues_recurrence_occurrence_uniq`,
		`ALTER TABLE issues DROP COLUMN recurrence_id`,
		`ALTER TABLE issues DROP COLUMN occurrence_key`,
		`DROP TABLE recurrences`,
		`ALTER TABLE issues DROP COLUMN metadata`,
		`ALTER TABLE issues DROP COLUMN revision`,
		`DROP INDEX IF EXISTS projects_area`,
		`ALTER TABLE projects DROP COLUMN metadata`,
		`ALTER TABLE projects DROP COLUMN revision`,
	}
	for _, sql := range stmts {
		_, err := raw.Exec(sql)
		require.NoErrorf(t, err, "drop v10 additions: %s", sql)
	}
}

// assertV8V9ShapeInternal mirrors assertV8V9Shape from fixtures_test.go.
func assertV8V9ShapeInternal(t *testing.T, raw *sql.DB) {
	t.Helper()
	assertTableShapeInternal(t, raw, "projects", []string{
		"id", "uid", "name", "created_at", "deleted_at",
	})
	assertTableShapeInternal(t, raw, "issues", []string{
		"id", "uid", "project_id", "short_id", "title", "body", "status",
		"closed_reason", "owner", "priority", "author",
		"created_at", "updated_at", "closed_at", "deleted_at",
	})
	assertTableShapeInternal(t, raw, "comments", []string{
		"id", "issue_id", "author", "body", "created_at",
	})
	assertTableShapeInternal(t, raw, "events", []string{
		"id", "uid", "origin_instance_uid", "project_id", "project_name",
		"issue_id", "issue_uid", "related_issue_id", "related_issue_uid",
		"type", "actor", "payload", "created_at",
	})
}

// deleteAutoSystemProjectInternal mirrors deleteAutoSystemProject.
func deleteAutoSystemProjectInternal(t *testing.T, raw *sql.DB) {
	t.Helper()
	_, err := raw.Exec(`DELETE FROM projects WHERE uid = ? AND name = ?`,
		db.SystemProjectUID, db.SystemProjectName)
	require.NoError(t, err)
}

// seedV9SchemaDBInternal builds a SQLite DB whose actual on-disk schema matches
// v9 (no metadata / revision columns, no recurrences). meta.schema_version is
// rewritten to '9' so exportForCutover's version-dispatch picks the pre-v10
// branches. Mirror of seedV9SchemaDB in cutover_test.go (package jsonl_test).
func seedV9SchemaDBInternal(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()

	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate v9 fixture db: %v", err)
	}
	require.NoError(t, d.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	dropV10AdditionsInternal(t, raw)
	assertV8V9ShapeInternal(t, raw)
	deleteAutoSystemProjectInternal(t, raw)

	const projectUID = "01HZZZZZZZZZZZZZZZZZZZZZZZ"
	const issueUID = "01HZZZZZZZZZZZZZZZZZZZZA01"
	_, err = raw.Exec(`INSERT INTO projects(id, uid, name) VALUES (1, ?, 'kata')`, projectUID)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO issues(id, uid, project_id, short_id, title, author)
		 VALUES (1, ?, 1, 'za01', 'v9 issue', 'tester')`, issueUID)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO events(uid, origin_instance_uid, project_id, project_name, issue_id, type, actor, payload)
		 VALUES ('01HZZZZZZZZZZZZZZZZZEVAL01', '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'kata', 1, 'issue.created', 'tester', '{}')`)
	require.NoError(t, err)

	_, err = raw.Exec(`UPDATE meta SET value='9' WHERE key='schema_version'`)
	require.NoError(t, err)
}

// openImportTargetDBInternal mirrors openImportTargetDB.
func openImportTargetDBInternal(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "target.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		_ = d.Close()
		t.Fatalf("migrate import target db: %v", err)
	}
	return d
}

// TestExportForCutoverPreV10NoMissingColumnError pins the v8/v9 export
// projection. A real v9 source DB has no metadata / revision / recurrence_id /
// occurrence_key columns; exportForCutover must omit all of them. Before this
// guard, the export against a v9 source produced "no such column: metadata"
// during the JSONL cutover.
func TestExportForCutoverPreV10NoMissingColumnError(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV9SchemaDBInternal(t, path)

	d, err := sqlitestore.Open(ctx, path, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	var buf bytes.Buffer
	err = exportForCutover(ctx, d, &buf, ExportOptions{IncludeDeleted: true})
	require.NoError(t, err, "export must not reference v10-only columns when source is v9")

	records := decodeJSONLLinesInternal(t, buf.Bytes())

	var sawIssue, sawProject, sawEvent, sawRecurrence bool
	for _, rec := range records {
		data, _ := rec["data"].(map[string]any)
		switch rec["kind"] {
		case "issue":
			sawIssue = true
			assert.NotContains(t, data, "recurrence_id", "v9 issue export must omit recurrence_id")
			assert.NotContains(t, data, "recurrence_uid", "v9 issue export must omit recurrence_uid")
			assert.NotContains(t, data, "occurrence_key", "v9 issue export must omit occurrence_key")
			assert.NotContains(t, data, "metadata", "v9 issue export must omit metadata (v10 column)")
			assert.NotContains(t, data, "revision", "v9 issue export must omit revision (v10 column)")
		case "project":
			sawProject = true
			assert.NotContains(t, data, "metadata", "v9 project export must omit metadata (v10 column)")
			assert.NotContains(t, data, "revision", "v9 project export must omit revision (v10 column)")
		case "event":
			sawEvent = true
			assert.Contains(t, data, "uid", "v9 event export must keep uid")
			assert.Contains(t, data, "origin_instance_uid",
				"v9 event export must keep origin_instance_uid")
		case "recurrence":
			sawRecurrence = true
		}
	}
	assert.True(t, sawIssue, "expected at least one issue record")
	assert.True(t, sawProject, "expected at least one project record")
	assert.True(t, sawEvent, "expected at least one event record")
	assert.False(t, sawRecurrence, "v9 export must not emit recurrence records")
}

// TestExportForCutoverPreV10RoundtripsThroughImport pins the end-to-end
// cutover path: a v9-shape DB exports without error, and the JSONL imports
// cleanly into a fresh v10 target. The user-facing regression signal for the
// v9 → v10 cutover.
func TestExportForCutoverPreV10RoundtripsThroughImport(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "src.db")
	seedV9SchemaDBInternal(t, srcPath)

	src, err := sqlitestore.Open(ctx, srcPath, db.ReadOnly())
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })

	var buf bytes.Buffer
	require.NoError(t, exportForCutover(ctx, src, &buf, ExportOptions{IncludeDeleted: true}))

	first := strings.SplitN(buf.String(), "\n", 2)[0]
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(first), &meta))
	data := meta["data"].(map[string]any)
	assert.Equal(t, "9", data["value"], "export_version should reflect source schema_version")

	target := openImportTargetDBInternal(t)
	require.NoError(t, Import(ctx, &buf, target))

	var issueCount, eventCount int
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM issues`).Scan(&issueCount))
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events`).Scan(&eventCount))
	assert.Equal(t, 1, issueCount, "v9 issue must survive cutover")
	assert.Equal(t, 1, eventCount, "v9 event must survive cutover")
}

// decodeJSONLLinesInternal decodes JSONL bytes into one map per line. Mirror
// of decodeJSONLLines in export_test.go (package jsonl_test, unreachable).
func decodeJSONLLinesInternal(t *testing.T, bs []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bs, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal(line, &rec))
		out = append(out, rec)
	}
	return out
}
