package sqlitestore_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// TestImportRecordValidate pins the tagged-union contract of ImportRecord
// directly. The end-to-end path runs through ImportReplay in later tests, but
// this table exercises Validate directly so unknown kinds, no-payload,
// multi-payload, and kind/payload mismatches each surface a clear error.
func TestImportRecordValidate(t *testing.T) {
	id := int64(1)
	cases := []struct {
		name    string
		rec     db.ImportRecord
		wantErr string
	}{
		{
			name:    "unknown kind",
			rec:     db.ImportRecord{Kind: "bogus", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "unknown kind",
		},
		{
			name:    "no payload",
			rec:     db.ImportRecord{Kind: "meta"},
			wantErr: "no payload set",
		},
		{
			name: "multiple payloads",
			rec: db.ImportRecord{
				Kind:    "meta",
				Meta:    &db.MetaKV{Key: "k", Value: "v"},
				Project: &db.ProjectExport{ID: id},
			},
			wantErr: "multiple payloads set",
		},
		{
			name:    "kind/payload mismatch",
			rec:     db.ImportRecord{Kind: "project", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "does not match",
		},
		{
			name:    "valid",
			rec:     db.ImportRecord{Kind: "meta", Meta: &db.MetaKV{Key: "k", Value: "v"}},
			wantErr: "",
		},
		{
			name: "valid federation_binding",
			rec: db.ImportRecord{
				Kind:              "federation_binding",
				FederationBinding: &db.FederationBindingExport{ProjectID: 1},
			},
			wantErr: "",
		},
		{
			name: "valid issue_claim",
			rec: db.ImportRecord{
				Kind:       "issue_claim",
				IssueClaim: &db.IssueClaimExport{ID: 1},
			},
			wantErr: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.rec.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestImportReplayInsertsEveryEntity is the smoke test for the round-trip.
// It seeds a source DB with one project + two issues + a link + a label + a
// comment, drains every export iterator into ImportRecord, replays into a
// fresh DB, then asserts table counts match the source for each table the
// fixture touches.
func TestImportReplayInsertsEveryEntity(t *testing.T) {
	ctx := context.Background()
	src, _, p, issue := setupTestIssue(t)
	other, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{ProjectID: p.ID, FromIssueID: issue.ID, ToIssueID: other.ID, Type: "blocks", Author: "a"})
	require.NoError(t, err)
	_, err = src.AddLabel(ctx, issue.ID, "urgent", "a")
	require.NoError(t, err)
	_, _, err = src.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "a", Body: "hi"})
	require.NoError(t, err)

	recs := collectImportRecords(t, ctx, src)

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))

	for _, table := range []string{"projects", "issues", "comments", "issue_labels", "links", "events"} {
		require.Equalf(t, tableCount(t, ctx, src, table), tableCount(t, ctx, dst, table), "%s row count", table)
	}
}

func TestImportReplayRejectsEventHashComputedBeforeResolvedIssueUID(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	recs := collectImportRecords(t, ctx, src)
	staleEventID, _, _ := makeFirstIssueEventHashStale(t, recs)

	dst := openTestDB(t)
	err := dst.ImportReplay(ctx, recs, db.ImportOptions{})

	require.Error(t, err)
	require.Contains(t, err.Error(), "event "+strconv.FormatInt(staleEventID, 10)+" content_hash mismatch")
	require.Equal(t, 0, tableCount(t, ctx, dst, "events"), "failed import must not persist stale events")
}

func TestImportReplayRecomputesLegacyEventHashAfterResolvedIssueUID(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	recs := collectImportRecords(t, ctx, src)
	staleEventID, wantHash, wantIssueUID := makeFirstIssueEventHashStale(t, recs)

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{
		RecomputeEventContentHash: true,
	}))

	var gotHash, gotIssueUID string
	require.NoError(t, dst.QueryRowContext(ctx,
		`SELECT content_hash, issue_uid FROM events WHERE id = ?`, staleEventID).Scan(&gotHash, &gotIssueUID))
	require.Equal(t, wantIssueUID, gotIssueUID)
	require.Equal(t, wantHash, gotHash)
}

// collectImportRecords drains the db export iterators into the current-shape
// ImportRecord slice (no version fills needed — the source is current schema).
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func collectImportRecords(t *testing.T, ctx context.Context, d *sqlitestore.Store) []db.ImportRecord {
	t.Helper()
	var recs []db.ImportRecord
	for rec, err := range d.ExportMeta(ctx) {
		require.NoError(t, err)
		m := rec
		recs = append(recs, db.ImportRecord{Kind: "meta", Meta: &m})
	}
	for rec, err := range d.ExportProjects(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "project", Project: &v})
	}
	for rec, err := range d.ExportProjectAliases(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "project_alias", Alias: &v})
	}
	for rec, err := range d.ExportRecurrences(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "recurrence", Recurrence: &v})
	}
	for rec, err := range d.ExportIssues(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "issue", Issue: &v})
	}
	for rec, err := range d.ExportComments(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "comment", Comment: &v})
	}
	for rec, err := range d.ExportIssueLabels(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "issue_label", Label: &v})
	}
	for rec, err := range d.ExportLinks(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "link", Link: &v})
	}
	for rec, err := range d.ExportImportMappings(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "import_mapping", ImportMapping: &v})
	}
	for rec, err := range d.ExportFederationBindings(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "federation_binding", FederationBinding: &v})
	}
	for rec, err := range d.ExportFederationSyncStatus(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "federation_sync_status", FederationSyncStatus: &v})
	}
	for rec, err := range d.ExportFederationQuarantine(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "federation_quarantine", FederationQuarantine: &v})
	}
	for rec, err := range d.ExportFederationEnrollments(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "federation_enrollment", FederationEnrollment: &v})
	}
	for rec, err := range d.ExportIssueClaims(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "issue_claim", IssueClaim: &v})
	}
	for rec, err := range d.ExportPendingClaimRequests(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "pending_claim_request", PendingClaimRequest: &v})
	}
	for rec, err := range d.ExportEvents(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "event", Event: &v})
	}
	for rec, err := range d.ExportPurgeLog(ctx, db.ExportFilter{IncludeDeleted: true}) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "purge_log", PurgeLog: &v})
	}
	for rec, err := range d.ExportSequences(ctx) {
		require.NoError(t, err)
		v := rec
		recs = append(recs, db.ImportRecord{Kind: "sqlite_sequence", Sequence: &v})
	}
	return recs
}

func makeFirstIssueEventHashStale(t *testing.T, recs []db.ImportRecord) (eventID int64, finalHash string, issueUID string) {
	t.Helper()
	for _, r := range recs {
		if r.Kind != db.ImportKindEvent || r.Event == nil || r.Event.IssueID == nil || r.Event.IssueUID == nil {
			continue
		}
		e := r.Event
		issueUID = *e.IssueUID
		e.IssueUID = nil
		staleHash, err := db.EventContentHash(db.EventHashInput{
			UID:               e.UID,
			OriginInstanceUID: e.OriginInstanceUID,
			ProjectUID:        e.ProjectUID,
			ProjectName:       e.ProjectName,
			IssueUID:          e.IssueUID,
			RelatedIssueUID:   e.RelatedIssueUID,
			Type:              e.Type,
			Actor:             e.Actor,
			HLCPhysicalMS:     e.HLCPhysicalMS,
			HLCCounter:        e.HLCCounter,
			CreatedAt:         e.CreatedAt,
			Payload:           e.Payload,
		})
		require.NoError(t, err)
		e.ContentHash = staleHash

		e.IssueUID = &issueUID
		finalHash, err = db.EventContentHash(db.EventHashInput{
			UID:               e.UID,
			OriginInstanceUID: e.OriginInstanceUID,
			ProjectUID:        e.ProjectUID,
			ProjectName:       e.ProjectName,
			IssueUID:          e.IssueUID,
			RelatedIssueUID:   e.RelatedIssueUID,
			Type:              e.Type,
			Actor:             e.Actor,
			HLCPhysicalMS:     e.HLCPhysicalMS,
			HLCCounter:        e.HLCCounter,
			CreatedAt:         e.CreatedAt,
			Payload:           e.Payload,
		})
		require.NoError(t, err)
		e.IssueUID = nil
		return e.ID, finalHash, issueUID
	}
	t.Fatal("fixture did not export an issue event with issue_uid")
	return 0, "", ""
}

//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func tableCount(t *testing.T, ctx context.Context, d *sqlitestore.Store, table string) int {
	t.Helper()
	var n int
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}

// TestImportReplayInstanceUID exercises the two instance-uid branches:
// default-mode replay adopts the source's instance_uid, while NewInstance
// preserves the target's (the value db.Open wrote on first open).
func TestImportReplayInstanceUID(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	srcUID := src.InstanceUID()
	recs := collectImportRecords(t, ctx, src)

	def := openTestDB(t)
	require.NoError(t, def.ImportReplay(ctx, recs, db.ImportOptions{}))
	require.Equal(t, srcUID, def.InstanceUID(), "default import adopts the source instance_uid")

	ni := openTestDB(t)
	localUID := ni.InstanceUID()
	require.NotEqual(t, srcUID, localUID)
	require.NoError(t, ni.ImportReplay(ctx, recs, db.ImportOptions{NewInstance: true}))
	require.Equal(t, localUID, ni.InstanceUID(), "NewInstance keeps the local instance_uid")
}

// TestImportReplayReconcilesSequence forces the imported issues
// sqlite_sequence record below MAX(id) and asserts reconcile repairs the
// persisted value. The naive "next live insert exceeds imported max" probe is
// vacuous on AUTOINCREMENT tables (SQLite never reuses an id <= MAX(rowid)
// while rows exist), so the assertion targets the persisted sqlite_sequence
// row directly — the value reconcile is uniquely responsible for raising.
func TestImportReplayReconcilesSequence(t *testing.T) {
	ctx := context.Background()
	src, _, p, _ := setupTestIssue(t)
	for i := 0; i < 3; i++ {
		_, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "x", Author: "a"})
		require.NoError(t, err)
	}
	recs := collectImportRecords(t, ctx, src)
	maxIssueID := tableMax(t, ctx, src, "issues")
	require.Greater(t, maxIssueID, int64(1), "fixture must have several issues")

	setSequenceRecord(recs, "issues", 1)

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))

	require.Equal(t, maxIssueID, storedSequence(t, ctx, dst, "issues"),
		"reconcile must raise the persisted sqlite_sequence to MAX(id)")
}

// setSequenceRecord rewrites the seq of the sqlite_sequence payload named name
// in place, failing the test if no such record exists.
func setSequenceRecord(recs []db.ImportRecord, name string, seq int64) {
	for _, r := range recs {
		if r.Kind == "sqlite_sequence" && r.Sequence != nil && r.Sequence.Name == name {
			r.Sequence.Seq = seq
			return
		}
	}
	panic("no sqlite_sequence record named " + name)
}

//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func storedSequence(t *testing.T, ctx context.Context, d *sqlitestore.Store, table string) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, d.QueryRowContext(ctx,
		`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq))
	return seq
}

//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func tableMax(t *testing.T, ctx context.Context, d *sqlitestore.Store, table string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, d.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM `+table).Scan(&n))
	return n
}

// TestImportReplayIsAtomic appends a project record whose uid collides with an
// existing one. uniqueProjectName renames the colliding name, but the uid
// stays, so the insert trips UNIQUE(projects.uid) mid-batch and the whole
// import must roll back. The duplicate-uid violation fires inside the insert
// loop (immediate constraint), which isolates the per-record rollback path
// cleanly — a deferred-FK violation would only surface at commit.
func TestImportReplayIsAtomic(t *testing.T) {
	ctx := context.Background()
	src, _, _, _ := setupTestIssue(t)
	recs := collectImportRecords(t, ctx, src)

	var dup db.ProjectExport
	for _, r := range recs {
		if r.Kind == "project" && r.Project.UID != db.SystemProjectUID {
			dup = *r.Project
			break
		}
	}
	require.NotEmpty(t, dup.UID, "fixture must contain a non-system project")
	dup.ID += 1000
	dup.Name += "-dup"
	recs = append(recs, db.ImportRecord{Kind: "project", Project: &dup})

	dst := openTestDB(t)
	err := dst.ImportReplay(ctx, recs, db.ImportOptions{})
	require.Error(t, err)
	// On a failed import the target must have only the auto-created system
	// project: every other row rolled back with the tx.
	var nonSystemProjects int
	require.NoError(t, dst.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM projects WHERE uid != ?`, db.SystemProjectUID).Scan(&nonSystemProjects))
	require.Equal(t, 0, nonSystemProjects, "a failed import commits no user projects")
	require.Equal(t, 0, tableCount(t, ctx, dst, "issues"))
}

// TestImportReplayRejectsMalformedRecord exercises the pre-transaction
// tagged-union validation: a malformed record (kind set but no payload) must
// fail with a slice-ordinal-bearing error and leave the target untouched.
func TestImportReplayRejectsMalformedRecord(t *testing.T) {
	ctx := context.Background()
	dst := openTestDB(t)
	recs := []db.ImportRecord{
		{Kind: "meta", Meta: &db.MetaKV{Key: "instance_uid", Value: "x"}},
		{Kind: "project"}, // malformed: no payload
	}
	err := dst.ImportReplay(ctx, recs, db.ImportOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "import record 1", "error names the slice ordinal")
	require.Contains(t, err.Error(), "no payload set")
	var n int
	require.NoError(t, dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE value = 'x'`).Scan(&n))
	require.Equal(t, 0, n, "no mutation on a malformed batch")
}

// TestFKColumnResolverResolvesIssuesProjectID exercises the newly-public
// resolver against the real schema: issues has FK columns project_id and
// recurrence_id, so at least one foreign_key_list index must resolve to a
// known column, and an out-of-range fkid must return "".
func TestFKColumnResolverResolvesIssuesProjectID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	resolver := sqlitestore.NewFKColumnResolver(d)

	var found string
	for fkid := 0; fkid < 4; fkid++ {
		col, err := resolver.Resolve(ctx, "issues", fkid)
		if err != nil {
			t.Fatalf("Resolve(issues, %d): %v", fkid, err)
		}
		if col == "project_id" || col == "recurrence_id" {
			found = col
		}
	}
	if found == "" {
		t.Fatal("expected an FK column of issues to resolve")
	}
	col, err := resolver.Resolve(ctx, "issues", 999)
	if err != nil {
		t.Fatalf("out-of-range Resolve: %v", err)
	}
	if col != "" {
		t.Fatalf("out-of-range fkid should resolve to empty, got %q", col)
	}
}
