package sqlitestore_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/uid"
)

// openTestDB opens a fresh database in a temporary directory and registers
// cleanup to close it when the test ends.
func openTestDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	d, _ := openTestDBWithPath(t)
	return d
}

// openTestDBWithPath is like openTestDB but also returns the on-disk path,
// useful for tests that need to reopen the database or pre-seed it.
func openTestDBWithPath(t *testing.T) (*sqlitestore.Store, string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "kata.db")
	d, err := sqlitestore.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.Migrate(context.Background()); err != nil {
		_ = d.Close()
		t.Fatalf("migrate test db: %v", err)
	}
	return d, path
}

// assertSchemaVersion checks that the schema_version row in meta matches the
// expected version.
func assertSchemaVersion(t *testing.T, d *sqlitestore.Store, expected int) {
	t.Helper()
	var got string
	require.NoError(t, d.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&got))
	assert.Equal(t, strconv.Itoa(expected), got)
}

// createKataProject creates the canonical "kata" project used by tests that
// need a project to exist but don't care about its name.
func createKataProject(ctx context.Context, t *testing.T, d *sqlitestore.Store) db.Project {
	t.Helper()
	return createProject(ctx, t, d, "kata")
}

// createProject creates a project with the given name, asserting no error.
func createProject(ctx context.Context, t *testing.T, d *sqlitestore.Store, name string) db.Project {
	t.Helper()
	p, err := d.CreateProject(ctx, name)
	require.NoError(t, err)
	return p
}

// setupTestProject opens a fresh database and creates a generic "p" project.
// Returns the db, a fresh context, and the project.
func setupTestProject(t *testing.T) (*sqlitestore.Store, context.Context, db.Project) {
	t.Helper()
	d := openTestDB(t)
	ctx := context.Background()
	p, err := d.CreateProject(ctx, "p")
	require.NoError(t, err)
	return d, ctx, p
}

// unmarshalPayload decodes a JSON event payload into T and returns it.
func unmarshalPayload[T any](t *testing.T, payload string) T {
	t.Helper()
	var dest T
	require.NoError(t, json.Unmarshal([]byte(payload), &dest))
	return dest
}

// makeIssue creates an issue under projectID with the given title and author,
// returning only the issue. Used when the caller doesn't need the create event.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func makeIssue(t *testing.T, ctx context.Context, d *sqlitestore.Store, projectID int64, title, author string) db.Issue {
	t.Helper()
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: author,
	})
	require.NoError(t, err)
	return issue
}

// addLabels applies the given labels to issueID, asserting no error on each.
func addLabels(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64, author string, labels ...string) {
	t.Helper()
	for _, lbl := range labels {
		_, err := d.AddLabel(ctx, issueID, lbl, author)
		require.NoError(t, err)
	}
}

// makeIssueWithLabels creates an issue and applies labels to it in one call.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func makeIssueWithLabels(t *testing.T, ctx context.Context, d *sqlitestore.Store, projectID int64, title, author string, labels ...string) db.Issue {
	t.Helper()
	issue := makeIssue(t, ctx, d, projectID, title, author)
	addLabels(ctx, t, d, issue.ID, author, labels...)
	return issue
}

// createTesterIssue creates an issue in projectID authored by "tester" with
// the given title.
func createTesterIssue(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, title string) (db.Issue, db.Event) {
	t.Helper()
	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID,
		Title:     title,
		Author:    "tester",
	})
	require.NoError(t, err)
	return issue, evt
}

// createTesterIssueWithBody creates an issue in projectID authored by "tester"
// with the given title and body. Used by search/FTS tests where the body
// content matters.
func createTesterIssueWithBody(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, title, body string) db.Issue {
	t.Helper()
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: projectID, Title: title, Body: body, Author: "tester",
	})
	require.NoError(t, err)
	return issue
}

// addTesterComment adds a comment authored by "tester" to issueID with the
// given body, asserting no error.
func addTesterComment(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64, body string) {
	t.Helper()
	_, _, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issueID, Author: "tester", Body: body,
	})
	require.NoError(t, err)
}

// createTesterIssues bulk-creates count issues in projectID, all authored by
// "tester" with title "x". Used to advance event ids when a test cares only
// about event sequencing, not issue content.
func createTesterIssues(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		createTesterIssue(ctx, t, d, projectID, "x")
	}
}

// setupTestIssue extends setupTestProject by also creating a single
// "tester"-authored issue with title "x". Returns db, context, project, and
// issue — the most common starting state for issue-mutation tests.
func setupTestIssue(t *testing.T) (*sqlitestore.Store, context.Context, db.Project, db.Issue) {
	t.Helper()
	d, ctx, p := setupTestProject(t)
	issue, _ := createTesterIssue(ctx, t, d, p.ID, "x")
	return d, ctx, p, issue
}

// setupSoftDeletedIssue extends setupTestIssue by soft-deleting the issue,
// the starting state for tests that exercise restore/purge of an
// already-deleted issue.
func setupSoftDeletedIssue(t *testing.T) (*sqlitestore.Store, context.Context, db.Project, db.Issue) {
	t.Helper()
	d, ctx, p, issue := setupTestIssue(t)
	_, _, _, err := d.SoftDeleteIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	return d, ctx, p, issue
}

// setupAssignedIssue extends setupTestIssue by assigning the issue to owner,
// the starting state for tests that exercise reassignment or unassignment.
func setupAssignedIssue(t *testing.T, owner string) (*sqlitestore.Store, context.Context, db.Project, db.Issue) {
	t.Helper()
	d, ctx, p, issue := setupTestIssue(t)
	_, _, _, err := d.UpdateOwner(ctx, issue.ID, &owner, "tester")
	require.NoError(t, err)
	return d, ctx, p, issue
}

// makeLink creates a link of the given type between fromID and toID under
// projectID, authored by "tester". Used for setup steps that aren't themselves
// the subject of an error assertion.
func makeLink(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID, fromID, toID int64, linkType string) db.Link {
	t.Helper()
	link, err := d.CreateLink(ctx, db.CreateLinkParams{
		ProjectID:   projectID,
		FromIssueID: fromID,
		ToIssueID:   toID,
		Type:        linkType,
		Author:      "tester",
	})
	require.NoError(t, err)
	return link
}

// attachAlias attaches a "git" alias to projectID, asserting no error. Used by
// tests that need an alias as setup but aren't asserting on AttachAlias's
// behavior.
func attachAlias(ctx context.Context, t *testing.T, d *sqlitestore.Store, projectID int64, identity, path string) db.ProjectAlias {
	t.Helper()
	a, err := d.AttachAlias(ctx, projectID, identity, "git", path)
	require.NoError(t, err)
	return a
}

// insertLegacyEvent stamps an event on the events table directly via SQL with
// a caller-supplied created_at value. Used by stats-parser tests that need to
// seed timestamps in the legacy zoned/space-separated layouts that
// CreateIssue won't produce.
func insertLegacyEvent(ctx context.Context, t *testing.T, d *sqlitestore.Store, p db.Project, issue db.Issue, eventType, createdAt string) {
	t.Helper()
	eventUID, err := uid.New()
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO events (
			uid, origin_instance_uid, project_id, project_name, issue_id,
			type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at
		)
		VALUES (?, (SELECT value FROM meta WHERE key='instance_uid'), ?, ?, ?, ?, 'tester', '{}', 1, 0,
		        '0000000000000000000000000000000000000000000000000000000000000000', ?)`,
		eventUID, p.ID, p.Name, issue.ID, eventType, createdAt)
	require.NoError(t, err)
}

// strPtr returns a pointer to s. Used by tests that need to pass an optional
// string parameter (e.g. issue owner) by address.
func strPtr(s string) *string { return &s }

// seedIssueInProject creates an issue in projectID with the given title and
// author, returning the created issue. Mirrors makeIssue but uses t as the
// first parameter to match the plan's helper signature.
func seedIssueInProject(t *testing.T, d *sqlitestore.Store, projectID int64, title, author string) db.Issue {
	t.Helper()
	issue, _, err := d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: author,
	})
	require.NoError(t, err)
	return issue
}

// seedRecurrenceInstance creates an issue and directly UPDATEs it to link to
// a recurrence with the given occurrence_key. Returns the issue's row id and
// UID. Mirrors the raw-UPDATE pattern used in jsonl roundtrip tests.
func seedRecurrenceInstance(
	t *testing.T, d *sqlitestore.Store, projectID, recurrenceID int64, occurrenceKey, title string,
) (int64, string) {
	t.Helper()
	iss, _, err := d.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: projectID, Title: title, Author: "tester",
	})
	require.NoError(t, err)
	_, err = d.ExecContext(context.Background(),
		`UPDATE issues SET recurrence_id = ?, occurrence_key = ? WHERE id = ?`,
		recurrenceID, occurrenceKey, iss.ID)
	require.NoError(t, err)
	return iss.ID, iss.UID
}

// seedLink creates a directed link of the given type between fromID and toID
// under projectID, authored by the given actor.
func seedLink(t *testing.T, d *sqlitestore.Store, projectID, fromID, toID int64, linkType, actor string) {
	t.Helper()
	_, err := d.CreateLink(context.Background(), db.CreateLinkParams{
		ProjectID:   projectID,
		FromIssueID: fromID,
		ToIssueID:   toID,
		Type:        linkType,
		Author:      actor,
	})
	require.NoError(t, err)
}

// seedRecurrence inserts a recurrences row directly via SQL and returns its id.
// Each test that calls seedRecurrence opens a fresh DB via openTestDB, so the
// hardcoded UID does not collide across test runs.
func seedRecurrence(t *testing.T, d *sqlitestore.Store, projectID int64, rule, dtstart, tz, title string) int64 {
	t.Helper()
	res, err := d.ExecContext(context.Background(), `
		INSERT INTO recurrences
		  (uid, project_id, rrule, dtstart, timezone, template_title, template_body,
		   template_labels, template_metadata, author)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"REC00000000000000000000001", projectID, rule, dtstart, tz, title, "",
		`[]`, `{}`, "tester")
	require.NoError(t, err)
	rid, err := res.LastInsertId()
	require.NoError(t, err)
	return rid
}

// injectIdempotencyKey rewrites the issue.created event payload for issueID
// to include the given idempotency key and fingerprint. Used by
// LookupIdempotency tests to seed idempotency state directly, in isolation
// from the CreateIssue idempotency path.
func injectIdempotencyKey(ctx context.Context, t *testing.T, d *sqlitestore.Store, issueID int64, key, fingerprint string) {
	t.Helper()
	_, err := d.ExecContext(ctx,
		`UPDATE events
		 SET payload = json_set(payload, '$.idempotency_key', ?, '$.idempotency_fingerprint', ?)
		 WHERE issue_id = ? AND type = 'issue.created'`,
		key, fingerprint, issueID)
	require.NoError(t, err)
}

// assertRowCount runs a count query and asserts the result equals expected.
// Reduces visual noise in tests that need to verify cascade-delete behavior.
func assertRowCount(ctx context.Context, t *testing.T, d *sqlitestore.Store, expected int, msg, query string, args ...any) {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRowContext(ctx, query, args...).Scan(&n))
	assert.Equal(t, expected, n, msg)
}

// assertEventCount asserts that the events table holds exactly expected rows
// of the given event type. Replaces the verbose
// `SELECT COUNT(*) FROM events WHERE type='X'` pattern that recurred across
// store_recurrences*_test.go and store_metadata_test.go.
func assertEventCount(t *testing.T, d *sqlitestore.Store, eventType string, expected int) {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM events WHERE type = ?`, eventType,
	).Scan(&n))
	assert.Equalf(t, expected, n, "events of type %q", eventType)
}

// setupRecurrence extends setupTestProject by also creating a recurrence
// inside the new project. Callers populate only the fields they care about
// in in (Rule / DTStart / Timezone / Template); ProjectID and a default
// Actor of "tester" are filled in automatically.
func setupRecurrence(t *testing.T, in db.CreateRecurrenceIn) (*sqlitestore.Store, context.Context, db.Project, db.Recurrence) {
	t.Helper()
	d, ctx, p := setupTestProject(t)
	in.ProjectID = p.ID
	if in.Actor == "" {
		in.Actor = "tester"
	}
	rec, err := d.CreateRecurrence(ctx, in)
	require.NoError(t, err)
	return d, ctx, p, rec
}
