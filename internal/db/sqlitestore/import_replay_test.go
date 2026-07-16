package sqlitestore_test

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
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
		{
			name: "valid issue_sync_binding",
			rec: db.ImportRecord{
				Kind:             db.ImportKindIssueSyncBinding,
				IssueSyncBinding: &db.IssueSyncBindingExport{ID: 1},
			},
			wantErr: "",
		},
		{
			name: "valid issue_sync_status",
			rec: db.ImportRecord{
				Kind:            db.ImportKindIssueSyncStatus,
				IssueSyncStatus: &db.IssueSyncStatusExport{BindingID: 1},
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

func TestImportReplayInsertsGitHubSyncState(t *testing.T) {
	ctx := context.Background()
	target := openTestDB(t)
	recs := []db.ImportRecord{
		{
			Kind: db.ImportKindProject,
			Project: &db.ProjectExport{
				ID:        1,
				UID:       "01HZZZZZZZZZZZZZZZZZZZZZ11",
				Name:      "example-project",
				CreatedAt: "2026-06-01T09:00:00.000Z",
				Metadata:  []byte(`{}`),
				Revision:  1,
			},
		},
		{
			Kind: db.ImportKindIssueSyncBinding,
			IssueSyncBinding: &db.IssueSyncBindingExport{
				ID:              7,
				ProjectID:       1,
				Provider:        "github",
				SourceKey:       "github:repo-node-example",
				RemoteID:        "repo-node-example",
				DisplayName:     "example-org/example-repo",
				Config:          []byte(`{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":42}`),
				Enabled:         true,
				IntervalSeconds: 900,
				LastCursorAt:    strPtr("2026-06-01T10:00:00.000Z"),
				CreatedAt:       "2026-06-01T09:00:00.000Z",
				UpdatedAt:       "2026-06-01T10:01:00.000Z",
			},
		},
		{
			Kind: db.ImportKindIssueSyncStatus,
			IssueSyncStatus: &db.IssueSyncStatusExport{
				BindingID:     7,
				ProjectID:     1,
				SyncStartedAt: strPtr("2026-06-01T09:58:00.000Z"),
				LastAttemptAt: strPtr("2026-06-01T09:58:00.000Z"),
				LastSuccessAt: strPtr("2026-06-01T10:00:00.000Z"),
				LastErrorAt:   strPtr("2026-06-01T10:02:00.000Z"),
				LastError:     strPtr("rate limited"),
				LastCreated:   2,
				LastUpdated:   3,
				LastUnchanged: 4,
				LastComments:  5,
			},
		},
	}
	require.NoError(t, target.ImportReplay(ctx, recs, db.ImportOptions{}))

	binding, err := target.IssueSyncBindingByProject(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(7), binding.ID)
	assert.Equal(t, "github", binding.Provider)
	assert.Equal(t, "github:repo-node-example", binding.SourceKey)
	assert.Equal(t, "repo-node-example", binding.RemoteID)
	assert.Equal(t, "example-org/example-repo", binding.DisplayName)
	assert.JSONEq(t, `{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":42}`, string(binding.Config))
	assert.False(t, binding.Enabled, "imported issue sync bindings must be re-enabled locally")
	require.NotNil(t, binding.LastCursorAt)
	assert.Equal(t, "2026-06-01T10:00:00.000Z", binding.LastCursorAt.UTC().Format("2006-01-02T15:04:05.000Z"))

	status, err := target.IssueSyncStatusByProject(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(7), status.BindingID)
	assert.Equal(t, "rate limited", status.LastError)
	assert.Equal(t, 2, status.LastCreated)
	assert.Equal(t, 3, status.LastUpdated)
	assert.Equal(t, 4, status.LastUnchanged)
	assert.Equal(t, 5, status.LastComments)
	require.NotNil(t, status.LastErrorAt)
	assert.Equal(t, "2026-06-01T10:02:00.000Z", status.LastErrorAt.UTC().Format("2006-01-02T15:04:05.000Z"))
}

func TestImportReplayCanPreserveIssueSyncBindingEnabledForTrustedCutover(t *testing.T) {
	ctx := context.Background()
	target := openTestDB(t)
	recs := []db.ImportRecord{
		{
			Kind: db.ImportKindProject,
			Project: &db.ProjectExport{
				ID:        1,
				UID:       "01HZZZZZZZZZZZZZZZZZZZZZ11",
				Name:      "example-project",
				CreatedAt: "2026-06-01T09:00:00.000Z",
				Metadata:  []byte(`{}`),
				Revision:  1,
			},
		},
		{
			Kind: db.ImportKindIssueSyncBinding,
			IssueSyncBinding: &db.IssueSyncBindingExport{
				ID:              7,
				ProjectID:       1,
				Provider:        "github",
				SourceKey:       "github:repo-node-example",
				RemoteID:        "repo-node-example",
				DisplayName:     "example-org/example-repo",
				Config:          []byte(`{"host":"github.com","owner":"example-org","repo":"example-repo","repo_id":42}`),
				Enabled:         true,
				IntervalSeconds: 900,
				CreatedAt:       "2026-06-01T09:00:00.000Z",
				UpdatedAt:       "2026-06-01T10:01:00.000Z",
			},
		},
	}
	require.NoError(t, target.ImportReplay(ctx, recs, db.ImportOptions{PreserveIssueSyncBindingEnabled: true}))

	binding, err := target.IssueSyncBindingByProject(ctx, 1)
	require.NoError(t, err)
	assert.True(t, binding.Enabled)
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
	_, err = src.CreateLink(ctx, db.CreateLinkParams{FromIssueID: issue.ID, ToIssueID: other.ID, Type: "blocks", Author: "a"})
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

// TestImportReplayRoundTripsProjectPurgeLog pins the project_purge_log cutover
// wiring: a purged project leaves a tombstone row that survives export →
// import into a fresh DB byte-for-byte on the columns that matter (uid, the
// snapshot identity, the counts, and the SSE reset cursor).
func TestImportReplayRoundTripsProjectPurgeLog(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	project, err := src.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	issue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID, Title: "doomed", Author: "tester",
	})
	require.NoError(t, err)
	_, _, err = src.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "tester", Body: "bye",
	})
	require.NoError(t, err)
	_, err = src.AddLabel(ctx, issue.ID, "urgent", "tester")
	require.NoError(t, err)

	// Archive then purge so a tombstone with non-zero counts exists to export.
	_, _, err = src.RemoveProject(ctx, db.RemoveProjectParams{ProjectID: project.ID, Actor: "tester", Force: true})
	require.NoError(t, err)
	reason := "cleanup"
	want, err := src.PurgeProject(ctx, db.PurgeProjectParams{
		ProjectID: project.ID, Actor: "tester", Reason: &reason,
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), want.IssueCount, "fixture must record a non-zero issue count")

	recs := collectImportRecords(t, ctx, src)
	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))

	require.Equal(t, 1, tableCount(t, ctx, dst, "project_purge_log"))
	got, err := scanRoundTripProjectPurgeLog(ctx, dst, want.ID)
	require.NoError(t, err)
	assert.Equal(t, want.UID, got.UID)
	assert.Equal(t, want.OriginInstanceUID, got.OriginInstanceUID)
	assert.Equal(t, want.ProjectID, got.ProjectID)
	require.NotNil(t, got.ProjectUID)
	assert.Equal(t, project.UID, *got.ProjectUID)
	assert.Equal(t, "spoke-project", got.ProjectName)
	assert.Equal(t, want.IssueCount, got.IssueCount)
	assert.Equal(t, want.EventCount, got.EventCount)
	assert.Equal(t, want.CommentCount, got.CommentCount)
	assert.Equal(t, want.LabelCount, got.LabelCount)
	assert.Equal(t, want.AliasCount, got.AliasCount)
	require.NotNil(t, got.PurgeResetAfterEventID)
	require.NotNil(t, want.PurgeResetAfterEventID)
	assert.Equal(t, *want.PurgeResetAfterEventID, *got.PurgeResetAfterEventID)
	assert.Equal(t, want.Actor, got.Actor)
	require.NotNil(t, got.Reason)
	assert.Equal(t, reason, *got.Reason)
}

func scanRoundTripProjectPurgeLog(ctx context.Context, d *sqlitestore.Store, id int64) (db.ProjectPurgeLogExport, error) {
	var pl db.ProjectPurgeLogExport
	err := d.QueryRowContext(ctx,
		`SELECT id, uid, origin_instance_uid, project_id, project_uid, project_name,
		        issue_count, event_count, alias_count, comment_count, link_count, label_count,
		        claim_count, pending_claim_request_count,
		        purge_reset_after_event_id, actor, reason
		   FROM project_purge_log WHERE id = ?`, id).Scan(
		&pl.ID, &pl.UID, &pl.OriginInstanceUID, &pl.ProjectID, &pl.ProjectUID, &pl.ProjectName,
		&pl.IssueCount, &pl.EventCount, &pl.AliasCount, &pl.CommentCount, &pl.LinkCount, &pl.LabelCount,
		&pl.ClaimCount, &pl.PendingClaimRequestCount,
		&pl.PurgeResetAfterEventID, &pl.Actor, &pl.Reason)
	return pl, err
}

// TestImportReplay_SkipsDanglingCrossProjectLink pins the missing-peer rule:
// a project-filtered envelope can carry a link whose peer issue is omitted;
// import skips it (reported, not fatal) instead of failing the FK pass, and
// importing the peer project's envelope later self-heals the edge once.
func TestImportReplay_SkipsDanglingCrossProjectLink(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	alpha, err := src.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := src.CreateProject(ctx, "beta")
	require.NoError(t, err)
	alphaIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	betaIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	link, err := src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: alphaIssue.ID, ToIssueID: betaIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)

	// The alpha-filtered envelope carries the link (it touches alpha) but omits
	// the beta peer issue. Import must skip the dangling edge, not fail the FK
	// pass.
	alphaRecs := collectImportRecordsForProject(t, ctx, src, alpha.ID)
	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, alphaRecs, db.ImportOptions{}),
		"a dangling cross-project link must be skipped, not fail the import")
	require.Equal(t, 0, tableCount(t, ctx, dst, "links"),
		"the link whose peer issue is omitted must not be inserted")

	// Self-heal: a whole-DB envelope (both peers present) lands the edge exactly
	// once. ImportReplay is a whole-DB atomic replace, so the peer side
	// re-delivers the edge through a fresh full replay rather than an
	// incremental merge into the alpha target.
	wholeRecs := collectImportRecords(t, ctx, src)
	healed := openTestDB(t)
	require.NoError(t, healed.ImportReplay(ctx, wholeRecs, db.ImportOptions{}))
	var fromUID, toUID, linkType string
	require.NoError(t, healed.QueryRowContext(ctx,
		`SELECT from_issue_uid, to_issue_uid, type FROM links`).Scan(&fromUID, &toUID, &linkType))
	require.Equal(t, 1, tableCount(t, ctx, healed, "links"), "self-heal lands exactly one link row")
	require.Equal(t, alphaIssue.UID, fromUID)
	require.Equal(t, betaIssue.UID, toUID)
	require.Equal(t, link.Type, linkType)
}

// TestImportReplay_DedupesRepeatedCrossProjectLink pins the natural-key no-op:
// both envelopes of one source DB carry the same cross-project edge, so a
// replay batch can present the same link record twice. The second delivery
// must be skipped (equal natural key implies equal id under id-preservation),
// leaving exactly one row instead of tripping UNIQUE(id) or
// UNIQUE(from_issue_id, to_issue_id, type).
func TestImportReplay_DedupesRepeatedCrossProjectLink(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	alpha, err := src.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := src.CreateProject(ctx, "beta")
	require.NoError(t, err)
	alphaIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	betaIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: alphaIssue.ID, ToIssueID: betaIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)

	recs := collectImportRecords(t, ctx, src)
	var dupLink db.LinkExport
	for _, r := range recs {
		if r.Kind == db.ImportKindLink {
			dupLink = *r.Link
			break
		}
	}
	require.NotZero(t, dupLink.ID, "fixture must export a link record")
	recs = append(recs, db.ImportRecord{Kind: db.ImportKindLink, Link: &dupLink})

	dst := openTestDB(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}),
		"a repeated cross-project link record must be deduped, not collide")
	require.Equal(t, 1, tableCount(t, ctx, dst, "links"), "the repeated edge lands exactly once")
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

// collectImportRecords drains every current-schema export through the shared
// backend-neutral replay fixture collector.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func collectImportRecords(t *testing.T, ctx context.Context, d *sqlitestore.Store) []db.ImportRecord {
	t.Helper()
	records, err := dbtest.CollectImportRecords(ctx, d, db.ExportFilter{IncludeDeleted: true})
	require.NoError(t, err)
	return records
}

// collectImportRecordsForProject builds the same project-scoped envelope used
// by shared storage conformance.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func collectImportRecordsForProject(t *testing.T, ctx context.Context, d *sqlitestore.Store, projectID int64) []db.ImportRecord {
	t.Helper()
	records, err := dbtest.CollectImportRecords(ctx, d, db.ExportFilter{
		ProjectID: &projectID, IncludeDeleted: true,
	})
	require.NoError(t, err)
	return records
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

// TestImportReplay_SkipsMappingForSkippedLink pins Finding B: a project-scoped
// envelope can carry an import_mapping (object_type=link) whose link record was
// skipped because the peer issue is absent. Without the fix, the FK check at
// commit fails. With the fix, the mapping is silently skipped and a note is
// emitted on stderr.
func TestImportReplay_SkipsMappingForSkippedLink(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	spoke, err := src.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hub, err := src.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	spokeIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: spoke.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	hubIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: hub.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	link, err := src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: spokeIssue.ID, ToIssueID: hubIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)
	_, err = src.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source:     "ext-tracker",
		ExternalID: "ext-link-1",
		ObjectType: "link",
		ProjectID:  spoke.ID,
		IssueID:    &spokeIssue.ID,
		LinkID:     &link.ID,
	})
	require.NoError(t, err)

	// The spoke-scoped envelope carries the link (it touches spoke) and the
	// import_mapping referencing it, but omits the hub peer issue. The link gets
	// skipped; without the fix, the import_mapping INSERT then FK-fails the replay.
	spokeRecs := collectImportRecordsForProject(t, ctx, src, spoke.ID)
	dst := openTestDB(t)
	stderr, restore := captureStderr(t)
	err = dst.ImportReplay(ctx, spokeRecs, db.ImportOptions{})
	restore()
	require.NoError(t, err, "import_mapping referencing a skipped link must not fail the replay")
	require.Equal(t, 0, tableCount(t, ctx, dst, "links"), "skipped link must not be inserted")
	require.Equal(t, 0, tableCount(t, ctx, dst, "import_mappings"), "mapping for skipped link must not be inserted")
	out := stderr.String()
	assert.Contains(t, out, "skipped 1 link record(s) whose peer issue is not in this envelope or database")
	assert.Contains(t, out, "skipped 1 import mapping record(s) referencing skipped link(s)")
}

// captureStderr redirects os.Stderr to an in-memory buffer for the duration of
// the test. The returned restore function reverts os.Stderr and drains any
// remaining pipe data into the buffer. Use the buffer for assertions.
// A t.Cleanup guard ensures restore runs even if the test calls t.Fatal early.
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

// TestImportReplay_StderrNotesMissingPeerOnly pins that the missing-peer
// aggregate note is emitted when only missing-peer skips occur, and the
// duplicate-skip note is absent.
func TestImportReplay_StderrNotesMissingPeerOnly(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	alpha, err := src.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := src.CreateProject(ctx, "beta")
	require.NoError(t, err)
	aIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	bIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: aIssue.ID, ToIssueID: bIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)

	alphaRecs := collectImportRecordsForProject(t, ctx, src, alpha.ID)
	dst := openTestDB(t)
	stderr, restore := captureStderr(t)
	require.NoError(t, dst.ImportReplay(ctx, alphaRecs, db.ImportOptions{}))
	restore()

	out := stderr.String()
	assert.Contains(t, out, "skipped 1 link record(s) whose peer issue is not in this envelope or database")
	assert.NotContains(t, out, "duplicate link record")
}

// TestImportReplay_StderrNotesDuplicateOnly pins that the duplicate-edge
// aggregate note is emitted when only duplicate skips occur, and the
// missing-peer note is absent.
func TestImportReplay_StderrNotesDuplicateOnly(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	alpha, err := src.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := src.CreateProject(ctx, "beta")
	require.NoError(t, err)
	aIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	bIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: aIssue.ID, ToIssueID: bIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)

	recs := collectImportRecords(t, ctx, src)
	var dupLink db.LinkExport
	for _, r := range recs {
		if r.Kind == db.ImportKindLink {
			dupLink = *r.Link
			break
		}
	}
	require.NotZero(t, dupLink.ID, "fixture must export a link record")
	recs = append(recs, db.ImportRecord{Kind: db.ImportKindLink, Link: &dupLink})

	dst := openTestDB(t)
	stderr, restore := captureStderr(t)
	require.NoError(t, dst.ImportReplay(ctx, recs, db.ImportOptions{}))
	restore()

	out := stderr.String()
	assert.Contains(t, out, "skipped 1 duplicate link record(s) (edge already present)")
	assert.NotContains(t, out, "peer issue is not in this envelope")
}

// TestImportReplay_StderrNotesMixed pins that both aggregate notes are emitted
// with the correct individual counts when both skip reasons occur in one replay.
func TestImportReplay_StderrNotesMixed(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	alpha, err := src.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := src.CreateProject(ctx, "beta")
	require.NoError(t, err)
	aIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "a"})
	require.NoError(t, err)
	bIssue, _, err := src.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "a"})
	require.NoError(t, err)
	_, err = src.CreateLink(ctx, db.CreateLinkParams{
		FromIssueID: aIssue.ID, ToIssueID: bIssue.ID, Type: "blocks", Author: "a",
	})
	require.NoError(t, err)

	// Whole-envelope so the link itself lands, then append a duplicate of that
	// same link (triggers skippedDup=1) plus two alpha-scoped link records whose
	// peer (bIssue) is absent (triggers skippedMissingPeer=2).
	wholeRecs := collectImportRecords(t, ctx, src)
	var realLink db.LinkExport
	for _, r := range wholeRecs {
		if r.Kind == db.ImportKindLink {
			realLink = *r.Link
			break
		}
	}
	require.NotZero(t, realLink.ID, "fixture must export a link record")

	// Two extra missing-peer links: fabricate records with the same from-issue
	// but non-existent to-issue ids so importLink returns linkSkipMissingPeer.
	ghost1 := db.LinkExport{ID: 900, FromIssueID: aIssue.ID, FromIssueUID: aIssue.UID, ToIssueID: 9901, ToIssueUID: "ghost-1", Type: "related", Author: "a", CreatedAt: realLink.CreatedAt}
	ghost2 := db.LinkExport{ID: 901, FromIssueID: aIssue.ID, FromIssueUID: aIssue.UID, ToIssueID: 9902, ToIssueUID: "ghost-2", Type: "related", Author: "a", CreatedAt: realLink.CreatedAt}

	mixed := append(wholeRecs,
		db.ImportRecord{Kind: db.ImportKindLink, Link: &realLink}, // duplicate
		db.ImportRecord{Kind: db.ImportKindLink, Link: &ghost1},   // missing peer
		db.ImportRecord{Kind: db.ImportKindLink, Link: &ghost2},   // missing peer
	)

	dst := openTestDB(t)
	stderr, restore := captureStderr(t)
	require.NoError(t, dst.ImportReplay(ctx, mixed, db.ImportOptions{}))
	restore()

	out := stderr.String()
	assert.Contains(t, out, "skipped 2 link record(s) whose peer issue is not in this envelope or database")
	assert.Contains(t, out, "skipped 1 duplicate link record(s) (edge already present)")
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

// TestImportReplay_RejectsControlCharacterProjectName pins import-side name
// validation: daemon create/rename reject names that can spoof terminal
// output (config.ValidateProjectName), and a crafted envelope must not
// bypass that gate — an imported project name is echoed by every CLI/TUI
// surface, including cross-project qualified refs. Storage-level
// CreateProject does not validate (the daemon handler does), which is
// exactly how a malicious envelope would be produced.
func TestImportReplay_RejectsControlCharacterProjectName(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)
	_, err := src.CreateProject(ctx, "evil\x1b]0;pwned\x07proj")
	require.NoError(t, err)

	recs := collectImportRecords(t, ctx, src)
	dst := openTestDB(t)
	err = dst.ImportReplay(ctx, recs, db.ImportOptions{})
	require.Error(t, err, "imported project names must pass creation-time validation")
	require.Contains(t, err.Error(), "non-printable")
}
