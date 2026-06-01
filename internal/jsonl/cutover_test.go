package jsonl_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/jsonl"
	"go.kenn.io/kata/internal/uid"
	_ "modernc.org/sqlite"
)

func TestAutoCutoverNoopsAtCurrentSchema(t *testing.T) {
	ctx, path := setupClosedTestDB(t)

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	assertCurrentSchemaVersion(t, path)
	assertNoCutoverTemps(t, path)
}

func TestAutoCutoverRefusesExistingTempFiles(t *testing.T) {
	ctx, path := setupClosedTestDB(t)
	require.NoError(t, os.WriteFile(path+".import.tmp.jsonl", []byte("partial"), 0o600))

	err := jsonl.AutoCutover(ctx, path)

	require.Error(t, err)
	assert.True(t, errors.Is(err, jsonl.ErrCutoverInProgress))
}

func TestAutoCutoverFailureLeavesSourceAndRemovesTemps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeVersionZeroDB(t, path)
	before, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	err = jsonl.AutoCutover(ctx, path)

	require.Error(t, err)
	after, readErr := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
	assertNoCutoverTemps(t, path)
}

func TestAutoCutoverUpgradesLegacyV1DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	writeLegacyV1DB(t, path)

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	assertCurrentSchemaVersion(t, path)
	var projectUID, issueUID, eventIssueUID string
	require.NoError(t, d.QueryRow(`SELECT uid FROM projects WHERE id = 1`).Scan(&projectUID))
	require.NoError(t, d.QueryRow(`SELECT uid FROM issues WHERE id = 1`).Scan(&issueUID))
	require.NoError(t, d.QueryRow(`SELECT issue_uid FROM events WHERE id = 1`).Scan(&eventIssueUID))
	assert.True(t, uid.Valid(projectUID))
	assert.True(t, uid.Valid(issueUID))
	assert.Equal(t, issueUID, eventIssueUID)
	assertNoCutoverTemps(t, path)
}

func TestAutoCutoverUpgradesLegacyV11DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")

	d := openMigratedTestDB(t, ctx, path)
	project, err := d.CreateProject(ctx, "legacy-v11")
	require.NoError(t, err)
	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "v11 issue",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID,
		Author:  "tester",
		Body:    "v11 comment",
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	trimCurrentDBToV11Shape(t, path)

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	assertCurrentSchemaVersion(t, path)
	got, err := upgraded.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, "v11 issue", got.Title)
	var commentUID, contentHash string
	require.NoError(t, upgraded.QueryRowContext(ctx,
		`SELECT uid FROM comments WHERE body = 'v11 comment'`).Scan(&commentUID))
	assert.True(t, uid.Valid(commentUID))
	require.NoError(t, upgraded.QueryRowContext(ctx,
		`SELECT content_hash FROM events WHERE type = 'issue.created' AND issue_uid = ?`, issue.UID).Scan(&contentHash))
	assert.Len(t, contentHash, 64)
}

func TestAutoCutover_ReconstructsAPITokensFromEvents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	d := openMigratedTestDB(t, ctx, path)
	active, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "active-token",
		Actor:          "wesm",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	revoked, _, err := d.CreateAPIToken(ctx, db.CreateAPITokenParams{
		PlaintextToken: "revoked-token",
		Actor:          "ci",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = d.RevokeAPIToken(ctx, revoked.ID, db.BootstrapActor)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx,
		`UPDATE meta SET value = ? WHERE key = 'schema_version'`,
		db.CurrentSchemaVersion()-1)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	upgraded, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })
	got, err := upgraded.ResolveAPIToken(ctx, "active-token")
	require.NoError(t, err)
	assert.Equal(t, active.ID, got.ID)
	assert.Equal(t, "wesm", got.Actor)
	_, err = upgraded.ResolveAPIToken(ctx, "revoked-token")
	assert.ErrorIs(t, err, db.ErrNotFound)
	tokens, err := upgraded.ListAPITokens(ctx)
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	assert.NotNil(t, tokens[1].RevokedAt)
}

func TestPeekSchemaVersion(t *testing.T) {
	ctx, path := setupClosedTestDB(t)
	assertCurrentSchemaVersion(t, path)

	noMeta := filepath.Join(t.TempDir(), "empty.db")
	raw, err := sql.Open("sqlite", noMeta)
	require.NoError(t, err)
	require.NoError(t, raw.PingContext(ctx))
	require.NoError(t, raw.Close())
	ver, err := sqlitestore.PeekSchemaVersion(ctx, noMeta)
	require.NoError(t, err)
	assert.Equal(t, 0, ver)
}

func TestClaimCutover_PreFederationImportLeavesClaimTablesEmpty(t *testing.T) {
	ctx := context.Background()
	dst := openImportTargetDB(t)
	body := buildJSONL(
		`{"kind":"meta","data":{"key":"export_version","value":"11"}}`,
		`{"kind":"meta","data":{"key":"instance_uid","value":"01HZZZZZZZZZZZZZZZZZZZZZ20"}}`,
		`{"kind":"project","data":{"id":1,"uid":"01HZZZZZZZZZZZZZZZZZZZZZ21","name":"legacy","metadata":{},"revision":1,"created_at":"2026-05-23T00:00:00.000Z"}}`,
	)

	require.NoError(t, jsonl.Import(ctx, strings.NewReader(body), dst))

	var claims, pending int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_claims`).Scan(&claims))
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_claim_requests`).Scan(&pending))
	assert.Equal(t, 0, claims)
	assert.Equal(t, 0, pending)
}

// TestRoundtripV4PreservesDeletedAt covers #24's projects.deleted_at column:
// an archived project in the source DB round-trips through JSONL with the
// timestamp intact, so kata-instance backups don't silently un-archive
// projects on restore.
func TestRoundtripV4PreservesDeletedAt(t *testing.T) {
	ctx := context.Background()
	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "archived")
	require.NoError(t, err)
	_, err = src.ExecContext(ctx,
		`UPDATE projects SET deleted_at = '2026-05-04T12:00:00.000Z' WHERE id = ?`, p.ID)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{IncludeDeleted: true}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst := openMigratedTestDB(t, ctx, dstPath)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	var got sql.NullTime
	require.NoError(t, dst.QueryRowContext(ctx,
		`SELECT deleted_at FROM projects WHERE id = ?`, p.ID).Scan(&got))
	require.True(t, got.Valid, "deleted_at must round-trip as a non-null timestamp")
	assert.Equal(t, "2026-05-04T12:00:00Z", got.Time.UTC().Format("2006-01-02T15:04:05Z"))
}

func writeVersionZeroDB(t *testing.T, path string) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	if _, err := d.Migrate(ctx); err != nil {
		_ = d.Close()
		t.Fatalf("migrate before stamping v0: %v", err)
	}
	_, err = d.ExecContext(ctx, `UPDATE meta SET value='0' WHERE key='schema_version'`)
	require.NoError(t, err)
	require.NoError(t, d.Close())
}

func assertNoCutoverTemps(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{".import.tmp.jsonl", ".import.tmp.db"} {
		_, err := os.Stat(path + suffix)
		assert.True(t, os.IsNotExist(err), path+suffix)
	}
}

// TestAutoCutover_HaltsOnUnknownFKClass: a source DB containing
// an FK violation outside the known orphan classes (a
// project_aliases row pointing at a missing project) refuses to
// cutover and reports actionable detail. The source DB is left
// untouched and no temp files remain.
func TestAutoCutover_HaltsOnUnknownFKClass(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV3DBWithOrphans(t, path, orphanSpec{OrphanProjectAlias: true})

	before, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	err = jsonl.AutoCutover(ctx, path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_aliases")
	assert.Contains(t, err.Error(), "parent=projects")
	assert.Contains(t, err.Error(), "column=project_id")

	after, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after, "source DB must not be mutated on preflight halt")
	assertNoCutoverTemps(t, path)
}

// TestAutoCutover_PrintsOrphanSummary: when cutover discards
// orphan rows, exactly one stderr line summarizes them, listing
// only nonzero classes in the fixed order events / comments /
// links / issue_labels. NULL-scrubbed events are NOT counted.
func TestAutoCutover_PrintsOrphanSummary(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV3DBWithOrphans(t, path, orphanSpec{
		OrphanComments:     2,
		OrphanLinks:        2,
		OrphanIssueLabels:  1,
		OrphanEventIssueID: 1,
		OrphanEventRelated: 1, // scrub, must not appear in summary
	})

	stderr, restore := captureStderr(t)
	err := jsonl.AutoCutover(ctx, path)
	_ = restore()
	require.NoError(t, err)

	// 1 dropped event + 2 dropped comments + 2 dropped links +
	// 1 dropped issue_label = 6. The related-only event is
	// scrubbed, not dropped, and is excluded from the summary.
	expected := "kata cutover: discarded 6 orphan rows from old DB (events: 1, comments: 2, links: 2, issue_labels: 1)\n"
	assert.Equal(t, expected, stderr.String())
}

// TestAutoCutover_NoSummaryWhenClean: no stderr output when the
// source DB has zero orphans, so existing operators upgrading a
// clean DB see no behavior change.
func TestAutoCutover_NoSummaryWhenClean(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV3DBWithOrphans(t, path, orphanSpec{}) // baseline only

	stderr, restore := captureStderr(t)
	err := jsonl.AutoCutover(ctx, path)
	_ = restore()
	require.NoError(t, err)
	assert.Empty(t, stderr.Bytes())
}

// TestAutoCutover_HaltErrorRendersNullRowid confirms the preflight
// halt message handles WITHOUT ROWID source tables. PRAGMA
// foreign_key_check returns NULL for the rowid column there, so the
// rendered detail uses the literal "NULL" rather than "0" or a
// formatting artifact.
func TestAutoCutover_HaltErrorRendersNullRowid(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV3DBWithOrphans(t, path, orphanSpec{})
	addWithoutRowidOrphan(t, path)

	err := jsonl.AutoCutover(ctx, path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "wr_child rowid=NULL parent=projects column=project_id")
	assertNoCutoverTemps(t, path)
}

// TestAutoCutover_V8HaltsOnOrphanImportMapping covers the v5+
// import_mappings code path that the v3 fixture cannot reach: an
// import_mappings row whose link_id points at a missing link gets
// classified as Unknown (preflight only knows the four issue-child
// classes) and halts cutover with actionable detail. Source DB and
// temp files are unchanged. This is the test that gives confidence
// the fix in this branch handles a v6/v7/v8 → v9 cutover where the
// orphan lives in import_mappings, not the four child tables the v3
// test exercises.
func TestAutoCutover_V8HaltsOnOrphanImportMapping(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV8DBWithOrphans(t, path, orphanSpec{OrphanImportMappingLink: 1})

	before, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)

	err = jsonl.AutoCutover(ctx, path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "import_mappings")
	assert.Contains(t, err.Error(), "parent=links")
	assert.Contains(t, err.Error(), "column=link_id")

	after, err := os.ReadFile(path) //nolint:gosec // test fixture under TempDir
	require.NoError(t, err)
	assert.Equal(t, before, after, "source DB must not be mutated on preflight halt")
	assertNoCutoverTemps(t, path)
}

// TestAutoCutover_V8DropsAllKnownOrphanClasses mirrors the v3 drop
// test on a v8 source. It exercises the V8+ events scrub branch
// (events with NULL issue_number column) and the v5+ exportImportMappings
// round-trip path, neither of which the v3 fixture reaches. This is
// the test that gives confidence the fix handles a v6/v7/v8 → v9
// cutover with orphans across the known classes.
func TestAutoCutover_V8DropsAllKnownOrphanClasses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV8DBWithOrphans(t, path, orphanSpec{
		OrphanComments:     2,
		OrphanLinks:        2,
		OrphanIssueLabels:  1,
		OrphanEventIssueID: 1,
		OrphanEventRelated: 1,
	})

	// Suppress the cutover summary stderr line; this test asserts data-
	// shape invariants. The summary is asserted by TestAutoCutover_PrintsOrphanSummary.
	_, restore := captureStderr(t)
	defer restore()
	require.NoError(t, jsonl.AutoCutover(ctx, path))

	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	assertCurrentSchemaVersion(t, path)

	var commentCount, linkCount, labelCount, importMappingCount int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments`).Scan(&commentCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM links`).Scan(&linkCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_labels`).Scan(&labelCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_mappings`).Scan(&importMappingCount))

	// Baseline: 1 valid comment, 1 valid link, 1 valid label, 4 valid
	// import_mappings. Orphans dropped entirely.
	assert.Equal(t, 1, commentCount, "valid comment survives; orphan comments dropped")
	assert.Equal(t, 1, linkCount, "valid link survives; orphan links dropped")
	assert.Equal(t, 1, labelCount, "valid label survives; orphan label dropped")
	assert.Equal(t, 4, importMappingCount, "all 4 valid import_mappings round-trip")

	// The related-only-orphan event is preserved with NULL related fields;
	// the issue_id-orphan event was dropped.
	var eventScrubbedCount int
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE type = 'issue.linked' AND related_issue_id IS NULL`).Scan(&eventScrubbedCount))
	assert.Equal(t, 1, eventScrubbedCount, "orphan related event survives with NULL related")

	assertNoCutoverTemps(t, path)
}

// TestAutoCutover_V8Clean: a v8 source with no orphans cuts over
// silently (no stderr summary line) and lands at the current schema.
// Confirms the fix is transparent to operators upgrading a clean v8
// DB to v9.
func TestAutoCutover_V8Clean(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV8DBWithOrphans(t, path, orphanSpec{})

	stderr, restore := captureStderr(t)
	err := jsonl.AutoCutover(ctx, path)
	_ = restore()
	require.NoError(t, err)
	assert.Empty(t, stderr.Bytes())
	assertCurrentSchemaVersion(t, path)
	assertNoCutoverTemps(t, path)
}

// TestAutoCutover_DropsAllKnownOrphanClasses: a source DB with
// orphans across all four known classes (events, comments,
// links, issue_labels) cuts over successfully. Orphan rows are
// dropped; events with valid issue_id but orphan related_issue_id
// are preserved with NULL related fields.
func TestAutoCutover_DropsAllKnownOrphanClasses(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kata.db")
	seedV3DBWithOrphans(t, path, orphanSpec{
		OrphanComments:     2,
		OrphanLinks:        2,
		OrphanIssueLabels:  1,
		OrphanEventIssueID: 1,
		OrphanEventRelated: 1,
	})

	// Suppress the cutover summary stderr line; this test asserts
	// data-shape invariants. The summary itself is asserted by
	// TestAutoCutover_PrintsOrphanSummary against the same seed.
	_, restore := captureStderr(t)
	defer restore()
	require.NoError(t, jsonl.AutoCutover(ctx, path))

	d, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	assertCurrentSchemaVersion(t, path)

	var commentCount, linkCount, labelCount, eventCount int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM comments`).Scan(&commentCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM links`).Scan(&linkCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_labels`).Scan(&labelCount))
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&eventCount))
	assert.Equal(t, 0, commentCount, "orphan comments should be dropped")
	assert.Equal(t, 0, linkCount, "orphan links should be dropped")
	assert.Equal(t, 0, labelCount, "orphan issue_labels should be dropped")

	var issueCount int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues`).Scan(&issueCount))
	assert.Equal(t, 3, issueCount, "the 3 seeded valid issues must survive cutover")

	// One event survives: the related-only orphan, with NULL
	// related fields. The issue_id-orphan event was dropped.
	assert.Equal(t, 1, eventCount, "only the related-only-orphan event should survive")
	var relatedID, relatedUID sql.NullString
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT related_issue_id, related_issue_uid FROM events`).Scan(&relatedID, &relatedUID))
	assert.False(t, relatedID.Valid, "related_issue_id must be NULL after scrub")
	assert.False(t, relatedUID.Valid, "related_issue_uid must be NULL after scrub")

	assertNoCutoverTemps(t, path)
}

// --- v3 → v4 ---

// TestV3ToV4CutoverPreservesProjects exercises the user-facing upgrade
// path: build a DB at the v3 schema, run AutoCutover, and verify
// existing projects survive with deleted_at NULL.
func TestV3ToV4CutoverPreservesProjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx := context.Background()

	writeLegacyV3DB(t, path)
	assertSchemaVersion(t, path, 3)

	require.NoError(t, jsonl.AutoCutover(ctx, path))

	assertCurrentSchemaVersion(t, path)

	d2, err := sqlitestore.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d2.Close() })

	var id int64
	var name string
	var deletedAt sql.NullString
	require.NoError(t, d2.QueryRowContext(ctx, `SELECT id, name, deleted_at FROM projects WHERE id=1`).Scan(&id, &name, &deletedAt))
	assert.Equal(t, int64(1), id)
	assert.Equal(t, "Proj A", name)
	assert.False(t, deletedAt.Valid, "deleted_at should be NULL for survived project")
}

// --- v7 → v8 ---

// Shared v7 issue UIDs for the event-payload rewrite tests. The last-4
// suffixes are unique so each issue lands at L=4 post-cutover.
const (
	v7UIDA = "01HZNQ7VFPK1XGD8R5MABCD4EX" // → short_id "d4ex"
	v7UIDB = "01HZNQ7VFPK1XGD8R5MABCDXYZ" // → "dxyz"
	v7UIDC = "01HZNQ7VFPK1XGD8R5MABCDQ99" // → "dq99"
)

func twoV7Issues() []v7Issue {
	return []v7Issue{
		{ProjectID: 1, ProjectName: "demo", UID: v7UIDA, Number: 1, Title: "a"},
		{ProjectID: 1, ProjectName: "demo", UID: v7UIDB, Number: 2, Title: "b"},
	}
}

// importEventPayload runs the cutover via jsonl.Import and returns the
// rewritten payload for the single matching event type. Each cutover
// event-rewrite test imports a small fixture of issues + one event and
// verifies the post-cutover payload shape.
func importEventPayload(t *testing.T, issues []v7Issue, ev v7Event) map[string]any {
	t.Helper()
	ctx := context.Background()
	target := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx,
		strings.NewReader(buildV7FixtureWith(t, issues, []v7Event{ev})), target))
	var payload string
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT payload FROM events WHERE type=?`, ev.Type).Scan(&payload))
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	return got
}

// TestCutoverV7_AssignsShortIDsInULIDOrder pins spec §4.2 and §8 for the
// v7→v8 step: two issues whose ULIDs share their last 4 chars are replayed in
// ULID-ascending order; the lexically-earlier ULID wins length-4 and the
// later one extends to length 5.
func TestCutoverV7_AssignsShortIDsInULIDOrder(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	// UIDs share the same last 4 chars ("d4ex"). The lexically-earlier UID
	// (no extra 'x' before the suffix) should win the length-4 short_id.
	jsonlInput := buildV7Fixture(t, []v7Issue{
		{ProjectID: 1, ProjectName: "demo", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Number: 1, Title: "first"},
		{ProjectID: 1, ProjectName: "demo", UID: "01HZNQ7VFPK1XGD8R5MABXD4EX", Number: 2, Title: "second"},
	})
	require.NoError(t, jsonl.Import(ctx, strings.NewReader(jsonlInput), target))

	proj, err := target.ProjectByName(ctx, "demo")
	require.NoError(t, err)
	rows, err := target.ListIssues(ctx, db.ListIssuesParams{ProjectID: proj.ID})
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byUID := map[string]db.Issue{}
	for _, r := range rows {
		byUID[r.UID] = r
	}
	first := byUID["01HZNQ7VFPK1XGD8R5MABCD4EX"]
	second := byUID["01HZNQ7VFPK1XGD8R5MABXD4EX"]
	assert.Equal(t, "d4ex", first.ShortID, "lexically-earlier UID wins length-4")
	assert.Equal(t, "xd4ex", second.ShortID, "second issue extends to length 5")
}

// TestCutoverV7_RejectsProjectNameWithHash pins spec §3: project names cannot
// contain '#' after cutover. The cutover must fail before any inserts with an
// error that names the offending project.
func TestCutoverV7_RejectsProjectNameWithHash(t *testing.T) {
	ctx := context.Background()
	target := openImportTargetDB(t)

	jsonlInput := buildV7Fixture(t, []v7Issue{
		{ProjectID: 1, ProjectName: "has#hash", UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Number: 1, Title: "ok"},
	})
	err := jsonl.Import(ctx, strings.NewReader(jsonlInput), target)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has#hash")
	assert.Contains(t, err.Error(), "must not contain '#'")

	// Cutover must fail BEFORE any inserts.
	var n int
	require.NoError(t, target.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE name <> ?`, db.SystemProjectName).Scan(&n))
	assert.Equal(t, 0, n, "cutover must fail before mutating target")
}

// TestCutoverV7_RewritesLinkEventPayload pins the event-payload rewrite for
// issue.linked / issue.unlinked. v7 events carried (from_number, to_number);
// v8 readers expect (from_short_id, from_uid, to_short_id, to_uid). Without
// the rewrite, post-cutover the TUI's Enter-on-event jump would silently
// no-op because to_short_id is missing.
func TestCutoverV7_RewritesLinkEventPayload(t *testing.T) {
	got := importEventPayload(t, twoV7Issues(), v7Event{
		ID: 1, ProjectID: 1, ProjectName: "demo", Type: "issue.linked",
		IssueID: 1, IssueUID: v7UIDA,
		PayloadJSON: `{"link_id":42,"type":"blocks","from_number":1,"to_number":2}`,
	})
	assert.Equal(t, "d4ex", got["from_short_id"])
	assert.Equal(t, v7UIDA, got["from_uid"])
	assert.Equal(t, "dxyz", got["to_short_id"])
	assert.Equal(t, v7UIDB, got["to_uid"])
	assert.NotContains(t, got, "from_number", "legacy from_number must be stripped")
	assert.NotContains(t, got, "to_number", "legacy to_number must be stripped")
	assert.EqualValues(t, 42, got["link_id"])
	assert.Equal(t, "blocks", got["type"])
}

// TestCutoverV7_RewritesCreatedEventLinks pins the rewrite for issue.created
// payloads whose `links` array carries v7 {type, to_number} entries: each
// entry must end up with to_short_id and to_issue_uid, with to_number
// stripped. Other payload fields (labels, idempotency_key) are untouched.
func TestCutoverV7_RewritesCreatedEventLinks(t *testing.T) {
	got := importEventPayload(t, twoV7Issues(), v7Event{
		ID: 1, ProjectID: 1, ProjectName: "demo", Type: "issue.created",
		IssueID: 2, IssueUID: v7UIDB,
		PayloadJSON: `{"labels":["bug"],"links":[{"type":"parent","to_number":1}],"idempotency_key":"k"}`,
	})
	links, ok := got["links"].([]any)
	require.True(t, ok, "links must remain an array")
	require.Len(t, links, 1)
	link := links[0].(map[string]any)
	assert.Equal(t, "parent", link["type"])
	assert.Equal(t, "d4ex", link["to_short_id"])
	assert.Equal(t, v7UIDA, link["to_issue_uid"])
	assert.NotContains(t, link, "to_number")
	// Untouched top-level fields.
	assert.Equal(t, "k", got["idempotency_key"])
	assert.Equal(t, []any{"bug"}, got["labels"])
}

// TestCutoverV7_RewritesLinksChangedPayload pins the rewrite for the
// aggregated edit event: v7 numeric parent_set / blocks_added become v8
// string short_ids paired with parallel *_uid / *_uids fields.
func TestCutoverV7_RewritesLinksChangedPayload(t *testing.T) {
	issues := append(twoV7Issues(),
		v7Issue{ProjectID: 1, ProjectName: "demo", UID: v7UIDC, Number: 3, Title: "c"})
	got := importEventPayload(t, issues, v7Event{
		ID: 1, ProjectID: 1, ProjectName: "demo", Type: "issue.links_changed",
		IssueID: 1, IssueUID: v7UIDA,
		PayloadJSON: `{"parent_set":2,"blocks_added":[3]}`,
	})
	assert.Equal(t, "dxyz", got["parent_set"])
	assert.Equal(t, v7UIDB, got["parent_set_uid"])
	assert.Equal(t, []any{"dq99"}, got["blocks_added"])
	assert.Equal(t, []any{v7UIDC}, got["blocks_added_uids"])
}

// TestCutover_PreservesStoredShortIDs pins spec §8.1: a round-trip on a v8
// database preserves stored short_ids byte-for-byte. The current-version
// import path must honor the source's short_id rather than re-deriving it.
func TestCutover_PreservesStoredShortIDs(t *testing.T) {
	ctx := context.Background()

	src := openExportTestDB(t)
	p, err := src.CreateProject(ctx, "demo")
	require.NoError(t, err)
	a, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "a",
		Author:    "tester",
	})
	require.NoError(t, err)
	b, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABXD4EX",
		Title:     "b",
		Author:    "tester",
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, jsonl.Export(ctx, src, &buf, jsonl.ExportOptions{}))

	dst := openImportTargetDB(t)
	require.NoError(t, jsonl.Import(ctx, bytes.NewReader(buf.Bytes()), dst))

	got, err := dst.IssueByUID(ctx, a.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, a.ShortID, got.ShortID)
	got, err = dst.IssueByUID(ctx, b.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, b.ShortID, got.ShortID)
}

// --- v9 → v10 ---
//
// The v9 pre-cutover export tests live in cutover_export_v9_internal_test.go
// (package jsonl) because the unexported exportForCutover (which they need to
// exercise the legacy projection) is not reachable from package jsonl_test
// after the backend-neutral Export refactor.

func trimCurrentDBToV11Shape(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = raw.Close() }()

	_, err = raw.ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;

		DROP TRIGGER IF EXISTS comments_ai_fts;
		DROP TRIGGER IF EXISTS comments_ad_fts;
		DROP TRIGGER IF EXISTS issues_ai_fts;
		DROP TRIGGER IF EXISTS issues_au_fts;
		DROP TRIGGER IF EXISTS issues_ad_fts;
		DROP INDEX IF EXISTS idx_comments_issue;
		ALTER TABLE comments RENAME TO comments_current;
		CREATE TABLE comments (
		  id         INTEGER PRIMARY KEY AUTOINCREMENT,
		  issue_id   INTEGER NOT NULL REFERENCES issues(id),
		  author     TEXT NOT NULL,
		  body       TEXT NOT NULL,
		  created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		  CHECK (length(trim(author)) > 0),
		  CHECK (length(trim(body))   > 0)
		);
		INSERT INTO comments(id, issue_id, author, body, created_at)
		  SELECT id, issue_id, author, body, created_at FROM comments_current;
		DROP TABLE comments_current;
		CREATE INDEX idx_comments_issue ON comments(issue_id, created_at);

		DROP INDEX IF EXISTS idx_events_project;
		DROP INDEX IF EXISTS idx_events_issue;
		DROP INDEX IF EXISTS idx_events_related;
		DROP INDEX IF EXISTS idx_events_issue_uid;
		DROP INDEX IF EXISTS idx_events_related_issue_uid;
		DROP INDEX IF EXISTS idx_events_origin_instance;
		DROP INDEX IF EXISTS idx_events_origin_project_id;
		DROP INDEX IF EXISTS idx_events_hlc;
		DROP INDEX IF EXISTS idx_events_content_hash;
		DROP INDEX IF EXISTS idx_events_idempotency;
		ALTER TABLE events RENAME TO events_current;
		CREATE TABLE events (
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
		);
		INSERT INTO events(
		  id, uid, origin_instance_uid, project_id, project_name, issue_id,
		  issue_uid, related_issue_id, related_issue_uid, type, actor, payload, created_at
		)
		  SELECT id, uid, origin_instance_uid, project_id, project_name, issue_id,
		         issue_uid, related_issue_id, related_issue_uid, type, actor, payload, created_at
		    FROM events_current;
		DROP TABLE events_current;
		CREATE INDEX idx_events_project ON events(project_id, id);
		CREATE INDEX idx_events_issue ON events(issue_id, id) WHERE issue_id IS NOT NULL;
		CREATE INDEX idx_events_related ON events(related_issue_id, id) WHERE related_issue_id IS NOT NULL;
		CREATE INDEX idx_events_issue_uid ON events(issue_uid) WHERE issue_uid IS NOT NULL;
		CREATE INDEX idx_events_related_issue_uid ON events(related_issue_uid) WHERE related_issue_uid IS NOT NULL;
		CREATE INDEX idx_events_origin_instance ON events(origin_instance_uid);
		CREATE INDEX idx_events_idempotency
		  ON events(project_id, json_extract(payload, '$.idempotency_key'), created_at)
		  WHERE type = 'issue.created' AND json_extract(payload, '$.idempotency_key') IS NOT NULL;

		UPDATE meta SET value='11' WHERE key='schema_version';
		PRAGMA foreign_keys = ON;
	`)
	require.NoError(t, err)
	assertTableShape(t, raw, "comments", []string{
		"id", "issue_id", "author", "body", "created_at",
	})
	assertTableShape(t, raw, "events", []string{
		"id", "uid", "origin_instance_uid", "project_id", "project_name",
		"issue_id", "issue_uid", "related_issue_id", "related_issue_uid",
		"type", "actor", "payload", "created_at",
	})
}
