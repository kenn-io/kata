package jsonl_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	got, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return got.UTC()
}

func assertTimePtrEqual(t *testing.T, want time.Time, got *time.Time) {
	t.Helper()
	require.NotNil(t, got)
	assert.True(t, want.Equal(*got), "want %s, got %s", want, *got)
}

// assertTableShape pins a fixture's table to an exact column set. Older-
// version fixtures (seedV8DBWithOrphans, seedV9SchemaDB) build the current
// schema and then DROP the v-N+1 additions by hand; without this check, a
// future schema bump that adds a column will silently slip through every
// older fixture's drop list and surface as "no such column: <col>" only on
// a real source DB. Tables are fixture-controlled, so the unparameterized
// pragma_table_info call is safe.
func assertTableShape(t *testing.T, raw *sql.DB, table string, expected []string) {
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

// validExportVersion is the standard export_version meta envelope tests
// prepend to bypass the decoder's initial version check.
const validExportVersion = `{"kind":"meta","data":{"key":"export_version","value":"1"}}`

// validV1ProjectRow is a canonical v1 project envelope with next_issue_number=1
// used by import tests that just need a referenceable parent project.
const validV1ProjectRow = `{"kind":"project","data":{"id":1,"identity":"github.com/wesm/kata","name":"kata","created_at":"2026-05-03T00:00:00.000Z","next_issue_number":1}}`

// buildJSONL joins envelope lines into valid JSONL: newline-separated with a
// trailing newline. Callers should not include trailing newlines in inputs.
func buildJSONL(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// importJSONL builds JSONL from lines and runs jsonl.Import against d,
// returning the import error so callers can assert success or failure.
func importJSONL(ctx context.Context, d *sqlitestore.Store, lines ...string) error {
	return jsonl.Import(ctx, strings.NewReader(buildJSONL(lines...)), d)
}

// setupClosedTestDB seeds a database file at the current schema version and
// closes it, returning a context and the on-disk path. Tests that exercise
// path-based entry points (AutoCutover, PeekSchemaVersion) need a database
// file with no open handles.
func setupClosedTestDB(t *testing.T) (context.Context, string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate closed test db: %v", err)
	}
	require.NoError(t, d.Close())
	return ctx, path
}

// assertCurrentSchemaVersion fails the test unless the database at path
// reports the schema version baked into this binary.
func assertCurrentSchemaVersion(t *testing.T, path string) {
	t.Helper()
	assertSchemaVersion(t, path, db.CurrentSchemaVersion())
}

// assertSchemaVersion fails the test unless the database at path reports
// the expected schema version.
func assertSchemaVersion(t *testing.T, path string, expected int) {
	t.Helper()
	ver, err := sqlitestore.PeekSchemaVersion(context.Background(), path)
	require.NoError(t, err)
	assert.Equal(t, expected, ver, "schema version mismatch")
}

// writeLegacyDBFromFixture creates a SQLite database at path by executing the
// SQL in testdata/<fixtureName>.
func writeLegacyDBFromFixture(t *testing.T, path, fixtureName string) {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", fixtureName)) //nolint:gosec // testdata path is constant per call site
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()
	_, err = raw.Exec(string(schema))
	require.NoError(t, err)
}

func writeLegacyV1DB(t *testing.T, path string) {
	t.Helper()
	writeLegacyDBFromFixture(t, path, "legacy_v1.sql")
}

func writeLegacyV2DB(t *testing.T, path string) {
	t.Helper()
	writeLegacyDBFromFixture(t, path, "legacy_v2.sql")
}

func writeLegacyV3DB(t *testing.T, path string) {
	t.Helper()
	writeLegacyDBFromFixture(t, path, "legacy_v3.sql")
}

// openCutoverDB writes a legacy database at a fresh temp path using seedFn,
// runs jsonl.AutoCutover, and opens the upgraded database. The opened handle
// is closed via t.Cleanup.
func openCutoverDB(ctx context.Context, t *testing.T, seedFn func(*testing.T, string)) *sqlitestore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedFn(t, path)
	require.NoError(t, jsonl.AutoCutover(ctx, path))
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// fetchInstanceUID returns the raw meta.instance_uid value stored in d.
func fetchInstanceUID(ctx context.Context, t *testing.T, d *sqlitestore.Store) string {
	t.Helper()
	var s string
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key='instance_uid'`).Scan(&s))
	return s
}

// exportToBuffer runs jsonl.Export with IncludeDeleted:true and returns the
// encoded bytes.
func exportToBuffer(ctx context.Context, t *testing.T, src *sqlitestore.Store) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{IncludeDeleted: true}))
	return &buf
}

type richJSONLFixture struct {
	DB      *sqlitestore.Store
	Project db.Project
}

// createTesterIssue creates an issue authored by "tester" with the given title,
// body, and labels. Used to cut the CreateIssue + require.NoError boilerplate
// from fixture builders that share the "tester" author convention.
func createTesterIssue(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, title, body string, labels ...string) db.Issue {
	t.Helper()
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID,
		Title:     title,
		Body:      body,
		Author:    "tester",
		Labels:    labels,
	})
	require.NoError(t, err)
	return issue
}

// addTesterComment posts a comment on issueID authored by "tester".
func addTesterComment(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64, body string) {
	t.Helper()
	_, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issueID,
		Author:  "tester",
		Body:    body,
	})
	require.NoError(t, err)
}

// attachAlias attaches an alias to projectID, asserting no error. The kind
// argument lets callers choose "git", "local", etc.
func attachAlias(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, identity, kind, path string) {
	t.Helper()
	_, err := d.AttachAlias(ctx, projectID, identity, kind, path)
	require.NoError(t, err)
}

func buildRichJSONLFixture(t *testing.T) richJSONLFixture {
	t.Helper()
	ctx := context.Background()
	d := openExportTestDB(t)

	p1, err := d.CreateProject(ctx, "kata")
	require.NoError(t, err)
	p2, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)
	attachAlias(ctx, t, d, p1.ID, "github.com/wesm/kata", "git", "/tmp/kata")
	attachAlias(ctx, t, d, p1.ID, "kata-local", "local", "/work/kata")
	attachAlias(ctx, t, d, p2.ID, "github.com/wesm/other", "git", "/tmp/other")

	login := createTesterIssue(ctx, t, d, p1.ID, "orchid login regression", "Safari login fails after the orchid rollout", "bug", "frontend")
	blocker := createTesterIssue(ctx, t, d, p1.ID, "api blocker", "Backend response blocks login", "backend")
	softDeleted := createTesterIssue(ctx, t, d, p1.ID, "soft deleted keeps FTS", "deleted but still exportable")
	purged := createTesterIssue(ctx, t, d, p1.ID, "purged audit trail", "purged body should leave purge_log only", "audit")
	createTesterIssue(ctx, t, d, p2.ID, "other project orchid", "cross project export coverage")
	binding := db.FederationBinding{
		ProjectID:            p2.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://127.0.0.1:7373",
		HubProjectID:         42,
		HubProjectUID:        p2.UID,
		ReplayHorizonEventID: 7,
		PullCursorEventID:    6,
		PushEnabled:          true,
		PushCursorEventID:    5,
		Enabled:              true,
	}
	_, err = d.UpsertFederationBinding(ctx, binding)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO federation_enrollments(token_hash, spoke_instance_uid, project_id, capabilities)
		VALUES(?, ?, ?, ?)`,
		strings.Repeat("c", 64), "01HZZZZZZZZZZZZZZZZZZZZZ03", p1.ID, "pull,push")
	require.NoError(t, err)

	addTesterComment(ctx, t, d, login.ID, "watermelon comment text")
	addTesterComment(ctx, t, d, purged.ID, "purged comment text")

	_, _, err = d.CreateLinkAndEvent(ctx, db.CreateLinkParams{
		ProjectID:   p1.ID,
		FromIssueID: blocker.ID,
		ToIssueID:   login.ID,
		Type:        "blocks",
		Author:      "tester",
	}, db.LinkEventParams{
		EventType:    "issue.linked",
		EventIssueID: blocker.ID,
		FromShortID:  blocker.ShortID, FromUID: blocker.UID,
		ToShortID: login.ShortID, ToUID: login.UID,
		Actor: "tester",
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, softDeleted.ID, "tester")
	require.NoError(t, err)
	reason := "roundtrip fixture"
	pl, err := d.PurgeIssue(ctx, purged.ID, "tester", &reason)
	require.NoError(t, err)
	require.NotNil(t, pl.PurgeResetAfterEventID)

	return richJSONLFixture{DB: d, Project: p1}
}

// v7Issue is a minimal issue fixture for buildV7Fixture. The fixture emits a
// v7-schema JSONL stream (export_version=7) where issue envelopes carry
// `number` and lack `short_id` — the shape that exercises the v7→v8 cutover.
type v7Issue struct {
	ProjectID   int64
	ProjectName string
	UID         string
	Number      int64
	Title       string
}

// v7Event lets cutover tests bundle one or more event envelopes into a
// v7 fixture without duplicating the envelope scaffolding per test. ID
// is the event's row id; ProjectID matches an issue. Type, IssueID,
// IssueUID, and Payload set the event body. PayloadJSON is the raw v7-
// shaped payload object the cutover should rewrite.
type v7Event struct {
	ID          int64
	ProjectID   int64
	ProjectName string
	Type        string
	IssueID     int64
	IssueUID    string
	PayloadJSON string
}

// buildV7Fixture returns a JSONL string at export_version=7 containing project
// envelopes (one per distinct ProjectID/ProjectName pair, in first-seen order)
// followed by issue envelopes in the given order. The output is intended for
// jsonl.Import, which detects export_version<8 and runs the v7→v8 cutover
// (derives short_ids in ULID-ascending order per project).
func buildV7Fixture(t *testing.T, issues []v7Issue) string {
	return buildV7FixtureWith(t, issues, nil)
}

// buildV7FixtureWith is the events-aware sibling of buildV7Fixture.
func buildV7FixtureWith(t *testing.T, issues []v7Issue, events []v7Event) string {
	t.Helper()
	var lines []string
	lines = append(lines, `{"kind":"meta","data":{"key":"export_version","value":"7"}}`)
	seenProject := map[int64]bool{}
	for _, iss := range issues {
		if seenProject[iss.ProjectID] {
			continue
		}
		seenProject[iss.ProjectID] = true
		// Use a fixed ULID base for projects so the fixture is deterministic;
		// the high byte tracks the project ID so distinct projects get
		// distinct UIDs.
		projectUID := "01HZZZZZZZZZZZZZZZZZZZZZZZ"
		switch iss.ProjectID {
		case 2:
			projectUID = "01HZZZZZZZZZZZZZZZZZZZZZ02"
		case 3:
			projectUID = "01HZZZZZZZZZZZZZZZZZZZZZ03"
		}
		lines = append(lines, fmt.Sprintf(
			`{"kind":"project","data":{"id":%d,"uid":%q,"name":%q,"created_at":"2026-05-03T00:00:00.000Z","next_issue_number":%d}}`,
			iss.ProjectID, projectUID, iss.ProjectName, len(issues)+1))
	}
	for _, iss := range issues {
		lines = append(lines, fmt.Sprintf(
			`{"kind":"issue","data":{"id":%d,"uid":%q,"project_id":%d,"number":%d,"title":%q,"body":"","status":"open","closed_reason":null,"owner":null,"author":"tester","created_at":"2026-05-03T00:00:01.000Z","updated_at":"2026-05-03T00:00:01.000Z","closed_at":null,"deleted_at":null}}`,
			iss.Number, iss.UID, iss.ProjectID, iss.Number, iss.Title))
	}
	for _, ev := range events {
		// origin_instance_uid is a fixed test value; events.uid is per-ID so
		// multiple events in one fixture don't collide on the UNIQUE.
		eventUID := fmt.Sprintf("01HZZZZZZZZZZZZZZZZZZZZE%02d", ev.ID%100)
		lines = append(lines, fmt.Sprintf(
			`{"kind":"event","data":{"id":%d,"project_id":%d,"project_name":%q,"issue_id":%d,"issue_uid":%q,"related_issue_id":null,"related_issue_uid":null,"type":%q,"actor":"tester","payload":%s,"created_at":"2026-05-03T00:00:03.000Z","uid":%q,"origin_instance_uid":"01HZZZZZZZZZZZZZZZZZZZZZ00"}}`,
			ev.ID, ev.ProjectID, ev.ProjectName, ev.IssueID, ev.IssueUID, ev.Type, ev.PayloadJSON, eventUID))
	}
	return buildJSONL(lines...)
}

// orphanSpec describes the orphan rows the seed helpers
// (seedV3DBWithOrphans, seedV8DBWithOrphans) should inject after the
// valid baseline rows. All counts default to 0. The v3 helper ignores
// OrphanImportMappingLink because the v3 schema has no import_mappings
// table; that field is honored only by seedV8DBWithOrphans.
type orphanSpec struct {
	OrphanComments          int  // comments referencing missing issue_id
	OrphanLinks             int  // links with one valid endpoint and one missing
	OrphanLinkBothEnds      int  // links with BOTH endpoints missing (dedup test)
	OrphanIssueLabels       int  // issue_labels referencing missing issue_id
	OrphanEventIssueID      int  // events with missing issue_id; related_issue_id is NULL
	OrphanEventRelated      int  // events with valid issue_id, missing related
	OrphanEventBoth         int  // events with BOTH columns missing (drop-precedence)
	OrphanProjectAlias      bool // single project_aliases row with missing project_id
	OrphanImportMappingLink int  // v5+ only: import_mappings rows whose link_id points at a missing link
}

// seedV3DBWithOrphans writes a v3-schema DB at path containing the existing
// proj-a project (id=1 from legacy_v3.sql), 3 valid issues, plus the orphan
// rows requested by spec. Orphans reference placeholder issue ID 999 (or
// 998/997 for second/third missing endpoint), which is never inserted. PRAGMA
// foreign_keys=OFF is used so the inserts succeed; post-cutover preflight then
// sees them.
func seedV3DBWithOrphans(t *testing.T, path string, spec orphanSpec) {
	t.Helper()
	writeLegacyV3DB(t, path)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	_, err = raw.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)

	// Seed 3 valid issues under project_id=1 (the proj-a project from the
	// legacy fixture). UIDs use only valid Crockford base32 characters
	// (uppercase, no I/L/O/U) so the v7→v8 short_id derivation accepts them.
	for i := 1; i <= 3; i++ {
		_, err = raw.Exec(`INSERT INTO issues (id, uid, project_id, number, title, author)
			VALUES (?, ?, 1, ?, ?, 'tester')`,
			i,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZZZZA%02d", i),
			i,
			fmt.Sprintf("issue %d", i),
		)
		require.NoError(t, err)
	}

	for i := 0; i < spec.OrphanComments; i++ {
		_, err = raw.Exec(`INSERT INTO comments (issue_id, author, body) VALUES (999, 'tester', ?)`,
			fmt.Sprintf("orphan comment %d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanLinks; i++ {
		_, err = raw.Exec(
			`INSERT INTO links (project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
			VALUES (1, 1, 999, '01HZZZZZZZZZZZZZZZZZZZZA01', '01HZZZZZZZZZZZZZZZZZZZZA99', 'related', 'tester')`)
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanLinkBothEnds; i++ {
		_, err = raw.Exec(
			`INSERT INTO links (project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
			VALUES (1, 998, 999, '01HZZZZZZZZZZZZZZZZZZZZA98', '01HZZZZZZZZZZZZZZZZZZZZA99', 'related', 'tester')`)
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanIssueLabels; i++ {
		_, err = raw.Exec(`INSERT INTO issue_labels (issue_id, label, author) VALUES (999, ?, 'tester')`,
			fmt.Sprintf("orphan-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanEventIssueID; i++ {
		_, err = raw.Exec(
			`INSERT INTO events (uid, origin_instance_uid, project_id, project_identity, issue_id, type, actor)
			VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'proj-a', 999, 'issue.created', 'tester')`,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZEV0A%02d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanEventRelated; i++ {
		_, err = raw.Exec(
			`INSERT INTO events (uid, origin_instance_uid, project_id, project_identity, issue_id, related_issue_id, type, actor)
			VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'proj-a', 1, 999, 'issue.linked', 'tester')`,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZEVRA%02d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanEventBoth; i++ {
		_, err = raw.Exec(
			`INSERT INTO events (uid, origin_instance_uid, project_id, project_identity, issue_id, related_issue_id, type, actor)
			VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'proj-a', 998, 999, 'issue.linked', 'tester')`,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZEVBA%02d", i))
		require.NoError(t, err)
	}
	if spec.OrphanProjectAlias {
		_, err = raw.Exec(`INSERT INTO project_aliases (project_id, alias_identity, alias_kind, root_path)
			VALUES (777, 'github.com/wesm/missing', 'git', '/tmp/missing')`)
		require.NoError(t, err)
	}
	_, err = raw.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
}

// dropV10Additions trims a freshly-bootstrapped current-schema DB back to
// the v8/v9 column shape: removes the recurrences table, the issues
// recurrence linkage, and metadata + revision on both issues and projects.
// SQLite supports DROP COLUMN since 3.35 (modernc.org/sqlite bundles a
// newer engine), but indexes referencing dropped columns must be removed
// first.
func dropV10Additions(t *testing.T, raw *sql.DB) {
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

// assertV8V9Shape verifies a fixture matches the real v8/v9 column set on
// the tables that diverge from current. Called after dropV10Additions so a
// future v11 column added without a corresponding drop fails loudly here
// rather than passing tests and crashing real-source cutover.
func assertV8V9Shape(t *testing.T, raw *sql.DB) {
	t.Helper()
	assertTableShape(t, raw, "projects", []string{
		"id", "uid", "name", "created_at", "deleted_at",
	})
	assertTableShape(t, raw, "issues", []string{
		"id", "uid", "project_id", "short_id", "title", "body", "status",
		"closed_reason", "owner", "priority", "author",
		"created_at", "updated_at", "closed_at", "deleted_at",
	})
	assertTableShape(t, raw, "comments", []string{
		"id", "issue_id", "author", "body", "created_at",
	})
	assertTableShape(t, raw, "events", []string{
		"id", "uid", "origin_instance_uid", "project_id", "project_name",
		"issue_id", "issue_uid", "related_issue_id", "related_issue_uid",
		"type", "actor", "payload", "created_at",
	})
}

// seedV8DBWithOrphans builds a source DB at meta.schema_version='8'
// containing a valid baseline (1 project, 3 issues, 1 link, 1 comment,
// 1 issue_label, 4 import_mappings) plus the orphan rows requested by
// spec. The on-disk schema is bootstrapped from the binary's current
// schema and then trimmed back to the real v8 shape (no metadata /
// revision / recurrence linkage / recurrences table), since v8 and v9
// share the same column set. This is the only fixture that exercises
// the v5+ import_mappings code path; the v3 fixture cannot reach it.
func seedV8DBWithOrphans(t *testing.T, path string, spec orphanSpec) {
	t.Helper()
	ctx := context.Background()

	// Phase 1: bring the DB to current via Open+Migrate so meta is populated
	// correctly, then close to release the handle before the raw connection
	// takes over.
	t.Setenv("KATA_HOME", t.TempDir())
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate fixture db: %v", err)
	}
	require.NoError(t, d.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	dropV10Additions(t, raw)
	assertV8V9Shape(t, raw)
	deleteAutoSystemProject(t, raw)

	// Phase 2: seed the valid baseline via raw SQL. Fixed UIDs keep the
	// fixture deterministic; short_ids satisfy the issues CHECK that
	// short_id = lower(substr(uid, 27 - length(short_id), length(short_id))).
	const projectUID = "01HZZZZZZZZZZZZZZZZZZZZZZZ"
	issueUIDs := []string{
		"01HZZZZZZZZZZZZZZZZZZZZA01",
		"01HZZZZZZZZZZZZZZZZZZZZA02",
		"01HZZZZZZZZZZZZZZZZZZZZA03",
	}
	issueShortIDs := []string{"za01", "za02", "za03"}

	_, err = raw.Exec(`INSERT INTO projects(id, uid, name) VALUES (1, ?, 'kata')`, projectUID)
	require.NoError(t, err)
	for i, uid := range issueUIDs {
		_, err = raw.Exec(
			`INSERT INTO issues(id, uid, project_id, short_id, title, author)
			 VALUES (?, ?, 1, ?, ?, 'tester')`,
			i+1, uid, issueShortIDs[i], fmt.Sprintf("issue %d", i+1))
		require.NoError(t, err)
	}
	_, err = raw.Exec(
		`INSERT INTO links(id, project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
		 VALUES (1, 1, 1, 2, ?, ?, 'related', 'tester')`,
		issueUIDs[0], issueUIDs[1])
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO comments(id, issue_id, author, body) VALUES (1, 1, 'tester', 'valid comment')`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO issue_labels(issue_id, label, author) VALUES (1, 'bug', 'tester')`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO events(uid, origin_instance_uid, project_id, project_name, issue_id, type, actor, payload)
		 VALUES ('01HZZZZZZZZZZZZZZZZZEVAL01', '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'kata', 1, 'issue.created', 'tester', '{}')`)
	require.NoError(t, err)

	// One valid import_mappings row per object_type. These are the
	// rows that exercise the v5+ exportImportMappings code path on the
	// happy path, alongside any orphans the spec requests.
	_, err = raw.Exec(
		`INSERT INTO import_mappings(source, external_id, object_type, project_id, issue_id) VALUES
		 ('beads', 'iss-1', 'issue', 1, 1)`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO import_mappings(source, external_id, object_type, project_id, issue_id, comment_id) VALUES
		 ('beads', 'cmt-1', 'comment', 1, 1, 1)`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO import_mappings(source, external_id, object_type, project_id, issue_id, link_id) VALUES
		 ('beads', 'lnk-1', 'link', 1, 1, 1)`)
	require.NoError(t, err)
	_, err = raw.Exec(
		`INSERT INTO import_mappings(source, external_id, object_type, project_id, issue_id, label) VALUES
		 ('beads', 'lbl-1', 'label', 1, 1, 'bug')`)
	require.NoError(t, err)

	// Phase 3: orphan injection with FKs disabled, schema_version
	// rewrite, then FKs re-enabled so PRAGMA foreign_key_check sees
	// every violation when the cutover preflight runs.
	_, err = raw.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)

	for i := 0; i < spec.OrphanComments; i++ {
		_, err = raw.Exec(`INSERT INTO comments(issue_id, author, body) VALUES (999, 'tester', ?)`,
			fmt.Sprintf("orphan comment %d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanLinks; i++ {
		// Vary to_issue_id per row so the (from, to, type) UNIQUE on
		// links doesn't collide across multiple orphan inserts.
		_, err = raw.Exec(
			`INSERT INTO links(project_id, from_issue_id, to_issue_id, from_issue_uid, to_issue_uid, type, author)
			 VALUES (1, 1, ?, ?, '01HZZZZZZZZZZZZZZZZZZZZA99', 'related', 'tester')`,
			900+i, issueUIDs[0])
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanIssueLabels; i++ {
		_, err = raw.Exec(`INSERT INTO issue_labels(issue_id, label, author) VALUES (999, ?, 'tester')`,
			fmt.Sprintf("orphan-%d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanEventIssueID; i++ {
		_, err = raw.Exec(
			`INSERT INTO events(uid, origin_instance_uid, project_id, project_name, issue_id, type, actor, payload)
			 VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'kata', 999, 'issue.created', 'tester', '{}')`,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZEV0A%02d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanEventRelated; i++ {
		_, err = raw.Exec(
			`INSERT INTO events(uid, origin_instance_uid, project_id, project_name, issue_id, related_issue_id, type, actor, payload)
			 VALUES (?, '01HZZZZZZZZZZZZZZZZZZZZZ00', 1, 'kata', 1, 999, 'issue.linked', 'tester', '{}')`,
			fmt.Sprintf("01HZZZZZZZZZZZZZZZZZEVRA%02d", i))
		require.NoError(t, err)
	}
	for i := 0; i < spec.OrphanImportMappingLink; i++ {
		_, err = raw.Exec(
			`INSERT INTO import_mappings(source, external_id, object_type, project_id, issue_id, link_id) VALUES
			 ('beads', ?, 'link', 1, 1, 999)`,
			fmt.Sprintf("orphan-link-%d", i))
		require.NoError(t, err)
	}
	if spec.OrphanProjectAlias {
		_, err = raw.Exec(`INSERT INTO project_aliases(project_id, alias_identity, alias_kind, root_path)
			VALUES (777, 'github.com/wesm/missing', 'git', '/tmp/missing')`)
		require.NoError(t, err)
	}

	_, err = raw.Exec(`UPDATE meta SET value='8' WHERE key='schema_version'`)
	require.NoError(t, err)
	_, err = raw.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
}

func deleteAutoSystemProject(t *testing.T, raw *sql.DB) {
	t.Helper()
	_, err := raw.Exec(`DELETE FROM projects WHERE uid = ? AND name = ?`,
		db.SystemProjectUID, db.SystemProjectName)
	require.NoError(t, err)
}

// addWithoutRowidOrphan augments an existing source DB with a tiny
// WITHOUT ROWID child table referencing projects(id) and inserts one
// orphan row whose project_id has no parent. PRAGMA foreign_key_check
// reports NULL for the rowid column on WITHOUT ROWID tables, so this
// fixture exercises the NULL-rowid scan path in PreflightSourceFKs.
func addWithoutRowidOrphan(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	_, err = raw.Exec(`PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = raw.Exec(`CREATE TABLE wr_child (
		key        TEXT NOT NULL PRIMARY KEY,
		project_id INTEGER NOT NULL REFERENCES projects(id)
	) WITHOUT ROWID`)
	require.NoError(t, err)
	_, err = raw.Exec(`INSERT INTO wr_child(key, project_id) VALUES ('orphan', 999)`)
	require.NoError(t, err)
	_, err = raw.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
}

// captureStderr redirects os.Stderr to an in-memory buffer for
// the duration of the test. The returned restore function reverts
// os.Stderr and copies any pending pipe data into the buffer.
// Use the buffer (not the restore return value) for assertions.
// A t.Cleanup guard ensures restore runs even if the test panics
// or calls t.Fatal before the manual restore call.
func captureStderr(t *testing.T) (*bytes.Buffer, func() *bytes.Buffer) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	original := os.Stderr
	os.Stderr = w
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	var once sync.Once
	restore := func() *bytes.Buffer {
		once.Do(func() {
			os.Stderr = original
			_ = w.Close()
			<-done
			_ = r.Close()
		})
		return buf
	}
	t.Cleanup(func() { _ = restore() })
	return buf, restore
}
