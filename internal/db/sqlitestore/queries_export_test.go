package sqlitestore_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// collectExport drains an export iterator, failing on the first error.
func collectExport[T any](t *testing.T, seq func(yield func(T, error) bool)) []T {
	t.Helper()
	var out []T
	for v, err := range seq {
		require.NoError(t, err)
		out = append(out, v)
	}
	return out
}

func TestExportMeta(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	// Open seeds meta (schema_version, instance_uid). Add a probe that sorts last.
	_, err := d.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('zzz_probe', 'v1')`)
	require.NoError(t, err)

	var got []db.MetaKV
	for rec, err := range d.ExportMeta(ctx) {
		require.NoError(t, err)
		got = append(got, rec)
	}

	require.NotEmpty(t, got)
	// ORDER BY key ASC: keys strictly ascending, probe last.
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Key, got[i].Key)
	}
	require.Equal(t, "zzz_probe", got[len(got)-1].Key)
	require.Equal(t, "v1", got[len(got)-1].Value)
}

func TestExportProjects(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	other, err := d.CreateProject(ctx, "other")
	require.NoError(t, err)

	// No filter: both projects (plus any auto-seeded system project),
	// ordered by id ASC.
	all := collectExport(t, d.ExportProjects(ctx, db.ExportFilter{}))
	require.GreaterOrEqual(t, len(all), 2)
	for i := 1; i < len(all); i++ {
		require.Less(t, all[i-1].ID, all[i].ID, "ORDER BY id ASC")
	}
	var got, gotOther *db.ProjectExport
	for i := range all {
		switch all[i].ID {
		case p.ID:
			got = &all[i]
		case other.ID:
			gotOther = &all[i]
		}
	}
	require.NotNil(t, got)
	require.NotNil(t, gotOther)
	require.Equal(t, p.UID, got.UID)
	require.True(t, json.Valid(got.Metadata), "metadata must be valid JSON")

	// ProjectID filter: only that project.
	one := collectExport(t, d.ExportProjects(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, one, 1)
	require.Equal(t, p.ID, one[0].ID)
}

func TestExportProjectsContextCanceledErrors(t *testing.T) {
	d, _, _ := setupTestProject(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force QueryContext to fail

	var sawErr error
	for _, err := range d.ExportProjects(ctx, db.ExportFilter{}) {
		sawErr = err
	}
	require.Error(t, sawErr, "a canceled context must surface as a terminal iterator error")
}

func TestExportProjectsEarlyBreak(t *testing.T) {
	d, ctx, _ := setupTestProject(t)
	_, err := d.CreateProject(ctx, "second")
	require.NoError(t, err)

	// Break after the first row; the deferred rows.Close must run cleanly.
	count := 0
	for _, err := range d.ExportProjects(ctx, db.ExportFilter{}) {
		require.NoError(t, err)
		count++
		break
	}
	require.Equal(t, 1, count)
}

func TestExportFederationBindings(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	_, err := d.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:     p.ID,
		Role:          "spoke",
		HubURL:        "https://example.test",
		HubProjectID:  1,
		HubProjectUID: "01HX00000000000000000HUBP1",
		Enabled:       true,
	})
	require.NoError(t, err)
	got := collectExport(t, d.ExportFederationBindings(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.Equal(t, "spoke", got[0].Role)
	require.True(t, got[0].Enabled)
}

func TestExportFederationSyncStatusEmpty(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	got := collectExport(t, d.ExportFederationSyncStatus(ctx, db.ExportFilter{ProjectID: &p.ID}))
	// No sync activity, so no rows; the test seeds nothing.
	require.Empty(t, got)
}

func TestExportFederationEnrollments(t *testing.T) {
	d, ctx, _ := setupTestProject(t)
	got := collectExport(t, d.ExportFederationEnrollments(ctx, db.ExportFilter{}))
	require.Empty(t, got, "no enrollments seeded")
}

func TestExportFederationQuarantineEmpty(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	got := collectExport(t, d.ExportFederationQuarantine(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Empty(t, got)
}

func TestExportIssueClaimsEmpty(t *testing.T) {
	d, ctx, _, _ := setupTestIssue(t)
	got := collectExport(t, d.ExportIssueClaims(ctx, db.ExportFilter{}))
	require.Empty(t, got)
}

func TestExportPendingClaimRequestsEmpty(t *testing.T) {
	d, ctx, _, _ := setupTestIssue(t)
	got := collectExport(t, d.ExportPendingClaimRequests(ctx, db.ExportFilter{}))
	require.Empty(t, got)
}

func TestExportSequences(t *testing.T) {
	d, _, _, _ := setupTestIssue(t) // creating rows advances sqlite_sequence
	ctx := context.Background()
	got := collectExport(t, d.ExportSequences(ctx))
	require.NotEmpty(t, got)
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Name, got[i].Name) // ORDER BY name ASC
	}
}

func TestExportProjectAliases(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, err := d.AttachAlias(ctx, p.ID, "alias-x", "git")
	require.NoError(t, err)

	got := collectExport(t, d.ExportProjectAliases(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, a.ID, got[0].ID)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.Equal(t, "git", got[0].AliasKind)
}

func TestExportComments(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "a", Body: "hello"})
	require.NoError(t, err)
	_, _, err = d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "a", Body: "world"})
	require.NoError(t, err)

	got := collectExport(t, d.ExportComments(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 2)
	require.Less(t, got[0].ID, got[1].ID)

	// Soft-delete the parent issue: default filter omits its comments.
	_, _, _, err = d.SoftDeleteIssue(ctx, issue.ID, "a")
	require.NoError(t, err)
	require.Empty(t, collectExport(t, d.ExportComments(ctx, db.ExportFilter{ProjectID: &p.ID})))
}

func TestExportIssueLabels(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, err := d.AddLabel(ctx, issue.ID, "alpha", "a")
	require.NoError(t, err)
	_, err = d.AddLabel(ctx, issue.ID, "beta", "a")
	require.NoError(t, err)

	got := collectExport(t, d.ExportIssueLabels(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 2)
	require.Equal(t, "alpha", got[0].Label)
	require.Equal(t, "beta", got[1].Label)
}

func TestExportImportMappings(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, err := d.UpsertImportMapping(ctx, db.ImportMappingParams{
		Source: "github", ExternalID: "ext-1", ObjectType: "issue",
		ProjectID: p.ID, IssueID: &issue.ID,
	})
	require.NoError(t, err)

	got := collectExport(t, d.ExportImportMappings(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.Equal(t, "github", got[0].Source)

	// Soft-deleting the underlying issue drops the mapping under default filter.
	_, _, _, err = d.SoftDeleteIssue(ctx, issue.ID, "a")
	require.NoError(t, err)
	require.Empty(t, collectExport(t, d.ExportImportMappings(ctx, db.ExportFilter{ProjectID: &p.ID})))
	// IncludeDeleted surfaces it again.
	require.Len(t, collectExport(t, d.ExportImportMappings(ctx, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true})), 1)
}

func TestExportPurgeLog(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	_, err := d.PurgeIssue(ctx, issue.ID, "x", nil)
	require.NoError(t, err)

	got := collectExport(t, d.ExportPurgeLog(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.NotEmpty(t, got[0].ProjectName)
}

func TestExportLinks(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	a, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "a", Author: "x"})
	require.NoError(t, err)
	b, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "b", Author: "x"})
	require.NoError(t, err)
	_, err = d.CreateLink(ctx, db.CreateLinkParams{FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "x"})
	require.NoError(t, err)

	got := collectExport(t, d.ExportLinks(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, a.ID, got[0].FromIssueID)
	require.Equal(t, b.ID, got[0].ToIssueID)

	// Soft-deleting an endpoint drops the link under the default filter.
	_, _, _, err = d.SoftDeleteIssue(ctx, b.ID, "x")
	require.NoError(t, err)
	require.Empty(t, collectExport(t, d.ExportLinks(ctx, db.ExportFilter{ProjectID: &p.ID})))
}

func TestExportRecurrences(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	rec, err := d.CreateRecurrence(ctx, db.CreateRecurrenceIn{
		ProjectID: p.ID, Rule: "FREQ=WEEKLY", DTStart: "2026-05-11", Timezone: "UTC",
		Template: db.RecurrenceTemplate{Title: "t"},
		Actor:    "tester",
	})
	require.NoError(t, err)

	got := collectExport(t, d.ExportRecurrences(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, rec.ID, got[0].ID)
	require.Equal(t, rec.UID, got[0].UID)
}

func TestExportEvents(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	// setupTestIssue creates an issue, which emits an issue.created event.
	evs := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.NotEmpty(t, evs)
	last := evs[len(evs)-1]
	require.Equal(t, p.ID, last.ProjectID)
	require.NotEmpty(t, last.ProjectName, "denormalized project_name must be populated")
	require.NotNil(t, last.IssueID)
	require.Equal(t, issue.ID, *last.IssueID)
	require.True(t, json.Valid(last.Payload))
	// ordered by id ascending
	for i := 1; i < len(evs); i++ {
		require.Less(t, evs[i-1].ID, evs[i].ID)
	}
}

// TestExportEvents_UIDOnlyPeerSoftDeleted regression-guards the federation
// shape where an event carries related_issue_uid without related_issue_id
// (because federation inserts events by UID). When the UID peer is soft-
// deleted, live export must drop non-links_changed events and scrub the
// related fields on links_changed events. Without the UID-aware JOIN and
// the UID branch of the orphan filter, the non-links_changed event leaks
// out with an orphan related_issue_uid and the links_changed event keeps a
// stale UID reference.
func TestExportEvents_UIDOnlyPeerSoftDeleted(t *testing.T) {
	d, ctx, p, peer := setupTestIssue(t)
	// Soft-delete the peer issue so its uid points at a deleted row.
	_, _, _, err := d.SoftDeleteIssue(ctx, peer.ID, "a")
	require.NoError(t, err)

	// Insert two raw events directly so they carry related_issue_uid
	// without related_issue_id (the federation shape).
	const fakeOrigin = "01ABCDEFGHJKMNPQRSTVWXYZ12"
	const fakeHash = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		                   related_issue_uid, type, actor, payload,
		                   hlc_physical_ms, hlc_counter, content_hash, created_at)
		VALUES (?, ?, ?, ?, ?, 'issue.foo', 'tester', '{}', 1, 0, ?, '2026-05-30T00:00:00.000Z')`,
		"01ABCDEFGHJKMNPQRSTVWXYZAA", fakeOrigin, p.ID, p.Name, peer.UID, fakeHash)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		                   related_issue_uid, type, actor, payload,
		                   hlc_physical_ms, hlc_counter, content_hash, created_at)
		VALUES (?, ?, ?, ?, ?, 'issue.links_changed', 'tester', '{}', 2, 0, ?, '2026-05-30T00:00:01.000Z')`,
		"01ABCDEFGHJKMNPQRSTVWXYZAB", fakeOrigin, p.ID, p.Name, peer.UID, fakeHash)
	require.NoError(t, err)

	live := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{ProjectID: &p.ID}))
	var fooSeen bool
	var linksScrubbed *db.EventExport
	for i, ev := range live {
		if ev.Type == "issue.foo" && ev.RelatedIssueUID != nil && *ev.RelatedIssueUID == peer.UID {
			fooSeen = true
		}
		if ev.Type == "issue.links_changed" && ev.UID == "01ABCDEFGHJKMNPQRSTVWXYZAB" {
			linksScrubbed = &live[i]
		}
	}
	require.False(t, fooSeen, "non-links_changed event with soft-deleted UID-only peer must be dropped from live export")
	require.NotNil(t, linksScrubbed, "links_changed event must remain in live export")
	require.Nil(t, linksScrubbed.RelatedIssueID, "related_issue_id must be scrubbed when UID-only peer is soft-deleted")
	require.Nil(t, linksScrubbed.RelatedIssueUID, "related_issue_uid must be scrubbed when UID-only peer is soft-deleted")
}

// TestExportEvents_ScrubsOmittedPeerRefs pins the cross-project scrub arm: a
// project-filtered envelope drops links across the project boundary (storage
// v16 lets a cross-project link export from both sides), so an issue.linked
// event whose peer issue lives in another project must surface with its
// related ids scrubbed — otherwise the envelope carries a peer the importer
// never receives. A whole-DB export keeps every peer reachable and must not
// scrub.
func TestExportEvents_ScrubsOmittedPeerRefs(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	alpha, err := d.CreateProject(ctx, "alpha")
	require.NoError(t, err)
	beta, err := d.CreateProject(ctx, "beta")
	require.NoError(t, err)
	alphaIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a", Author: "tester"})
	require.NoError(t, err)
	alphaPeer, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: alpha.ID, Title: "a2", Author: "tester"})
	require.NoError(t, err)
	betaIssue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: beta.ID, Title: "b", Author: "tester"})
	require.NoError(t, err)

	// A cross-project issue.linked event in alpha whose peer (related) issue
	// lives in beta. Seeded raw so the related_issue_id/_uid pair is explicit;
	// ExportEvents recomputes the content hash, so the placeholder is fine.
	const fakeOrigin = "01ABCDEFGHJKMNPQRSTVWXYZ12"
	const fakeHash = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		                   issue_id, issue_uid, related_issue_id, related_issue_uid,
		                   type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'issue.linked', 'tester', '{}', 5, 0, ?, '2026-05-30T00:00:00.000Z')`,
		"01ABCDEFGHJKMNPQRSTVWXYZAC", fakeOrigin, alpha.ID, alpha.Name,
		alphaIssue.ID, alphaIssue.UID, betaIssue.ID, betaIssue.UID, fakeHash)
	require.NoError(t, err)

	// A same-project issue.linked event in alpha whose peer also lives in alpha.
	// Under a project-filtered export this must NOT be scrubbed — the peer is
	// included in the same envelope. This control event guards against a
	// regression that over-scrubs same-project peers (e.g. a transposed
	// positional arg in the scrubCondition assembly).
	_, err = d.ExecContext(ctx, `
		INSERT INTO events(uid, origin_instance_uid, project_id, project_name,
		                   issue_id, issue_uid, related_issue_id, related_issue_uid,
		                   type, actor, payload, hlc_physical_ms, hlc_counter, content_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'issue.linked', 'tester', '{}', 6, 0, ?, '2026-05-30T00:00:01.000Z')`,
		"01ABCDEFGHJKMNPQRSTVWXYZAD", fakeOrigin, alpha.ID, alpha.Name,
		alphaIssue.ID, alphaIssue.UID, alphaPeer.ID, alphaPeer.UID, fakeHash)
	require.NoError(t, err)

	findEvent := func(evs []db.EventExport, uid string) *db.EventExport {
		for i := range evs {
			if evs[i].UID == uid {
				return &evs[i]
			}
		}
		return nil
	}

	filteredEvs := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{ProjectID: &alpha.ID}))

	// Alpha-filtered: the beta peer is omitted, so the related refs are scrubbed.
	filtered := findEvent(filteredEvs, "01ABCDEFGHJKMNPQRSTVWXYZAC")
	require.NotNil(t, filtered, "the alpha-scoped cross-project event must still be exported")
	require.Nil(t, filtered.RelatedIssueID, "related_issue_id must be scrubbed when the peer is in an omitted project")
	require.Nil(t, filtered.RelatedIssueUID, "related_issue_uid must be scrubbed when the peer is in an omitted project")

	// Alpha-filtered: the same-project alpha peer must NOT be scrubbed.
	filteredSame := findEvent(filteredEvs, "01ABCDEFGHJKMNPQRSTVWXYZAD")
	require.NotNil(t, filteredSame, "the same-project event must be exported under alpha filter")
	require.NotNil(t, filteredSame.RelatedIssueID, "related_issue_id must not be scrubbed for a same-project peer")
	require.Equal(t, alphaPeer.ID, *filteredSame.RelatedIssueID)
	require.NotNil(t, filteredSame.RelatedIssueUID, "related_issue_uid must not be scrubbed for a same-project peer")
	require.Equal(t, alphaPeer.UID, *filteredSame.RelatedIssueUID)

	// Whole-DB: every peer is reachable, so nothing is scrubbed.
	wholeEvs := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{}))
	whole := findEvent(wholeEvs, "01ABCDEFGHJKMNPQRSTVWXYZAC")
	require.NotNil(t, whole, "the event must be exported in a whole-DB export")
	require.NotNil(t, whole.RelatedIssueID, "whole-DB export must not scrub a present peer")
	require.Equal(t, betaIssue.ID, *whole.RelatedIssueID)
	require.NotNil(t, whole.RelatedIssueUID)
	require.Equal(t, betaIssue.UID, *whole.RelatedIssueUID)
}

func TestExportIssues(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)
	deleted, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: p.ID, Title: "d", Author: "a"})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, deleted.ID, "a")
	require.NoError(t, err)

	// Default filter excludes soft-deleted issues.
	live := collectExport(t, d.ExportIssues(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, live, 1)
	require.Equal(t, issue.ID, live[0].ID)
	require.Equal(t, issue.UID, live[0].UID)
	require.True(t, json.Valid(live[0].Metadata))

	// IncludeDeleted surfaces the soft-deleted issue too, ordered by id.
	all := collectExport(t, d.ExportIssues(ctx, db.ExportFilter{ProjectID: &p.ID, IncludeDeleted: true}))
	require.Len(t, all, 2)
	require.Equal(t, issue.ID, all[0].ID)
	require.Equal(t, deleted.ID, all[1].ID)
	require.NotNil(t, all[1].DeletedAt)
}

// TestExportEvents_MovedIssueEventsSurvive pins the moved-issue rule: kata
// move rehomes the issue row while its historical events stay in the source
// project, so the subject join must match by row id / UID alone. Requiring
// subject_issue.project_id = events.project_id made the orphan filter drop
// every event of a moved issue from exports.
func TestExportEvents_MovedIssueEventsSurvive(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	srcP, err := d.CreateProject(ctx, "src")
	require.NoError(t, err)
	tgtP, err := d.CreateProject(ctx, "target")
	require.NoError(t, err)
	issue := seedIssueInProject(t, d, srcP.ID, "Move me", "tester")
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID:       issue.ID,
		FromProjectID: srcP.ID,
		ToProjectID:   tgtP.ID,
		IfMatchRev:    1,
		Actor:         "tester",
	})
	require.NoError(t, err)

	find := func(recs []db.EventExport, evType string) *db.EventExport {
		for i := range recs {
			if recs[i].Type == evType {
				return &recs[i]
			}
		}
		return nil
	}

	// Full export keeps the moved issue's source-project events intact:
	// the subject row exists (in the target project), so issue_id stays.
	all := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{}))
	created := find(all, "issue.created")
	require.NotNil(t, created, "issue.created of a moved issue must export")
	require.Equal(t, srcP.ID, created.ProjectID, "events stay in the source project")
	require.NotNil(t, created.IssueID, "full export keeps the row reference")
	require.Equal(t, issue.ID, *created.IssueID)

	// A source-project-filtered envelope keeps the event too, but cannot
	// reference an issue row it does not contain: issue_id is scrubbed
	// (events.issue_id has an FK the importer would trip over) while
	// issue_uid — the content-hash identity — is retained, the same
	// UID-only shape federation-inserted events already use.
	filtered := collectExport(t, d.ExportEvents(ctx, db.ExportFilter{ProjectID: &srcP.ID}))
	fc := find(filtered, "issue.created")
	require.NotNil(t, fc, "source-project envelope must keep the moved issue's events")
	require.Nil(t, fc.IssueID, "filtered export must not carry a dangling issue_id")
	require.NotNil(t, fc.IssueUID)
	require.Equal(t, issue.UID, *fc.IssueUID)
}

// TestExportEvents_MovedIssueEventsSurviveReExport pins the second
// generation: a source-project envelope preserves a moved issue's events as
// UID-only records (issue_id scrubbed, issue_uid kept), so after importing
// that envelope the restored DB holds events whose subject row never
// existed locally. The next export must keep them — the subject liveness
// filter may only drop id-keyed orphans (purge leftovers) and, on live-only
// exports, subjects that joined a soft-deleted row. Requiring a joined row
// for every UID-keyed event made first-generation preservation silently
// decay on re-export.
func TestExportEvents_MovedIssueEventsSurviveReExport(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	srcP, err := d.CreateProject(ctx, "src")
	require.NoError(t, err)
	tgtP, err := d.CreateProject(ctx, "target")
	require.NoError(t, err)
	issue := seedIssueInProject(t, d, srcP.ID, "Move me", "tester")
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
		IssueID:       issue.ID,
		FromProjectID: srcP.ID,
		ToProjectID:   tgtP.ID,
		IfMatchRev:    1,
		Actor:         "tester",
	})
	require.NoError(t, err)

	// Generation 1: source-project envelope → fresh DB. The moved issue's
	// events land UID-only (their issue row lives in the omitted project).
	recs := collectImportRecordsForProject(t, ctx, d, srcP.ID)
	restored := openTestDB(t)
	require.NoError(t, restored.ImportReplay(ctx, recs, db.ImportOptions{}))

	find := func(recs []db.EventExport, evType string) *db.EventExport {
		for i := range recs {
			if recs[i].Type == evType {
				return &recs[i]
			}
		}
		return nil
	}

	// Generation 2: re-export the restored DB, filtered and unfiltered.
	again := collectExport(t, restored.ExportEvents(ctx, db.ExportFilter{ProjectID: &srcP.ID, IncludeDeleted: true}))
	created := find(again, "issue.created")
	require.NotNil(t, created, "UID-only moved-issue events must survive re-export")
	require.Nil(t, created.IssueID)
	require.NotNil(t, created.IssueUID)
	require.Equal(t, issue.UID, *created.IssueUID)

	full := collectExport(t, restored.ExportEvents(ctx, db.ExportFilter{}))
	require.NotNil(t, find(full, "issue.created"),
		"unfiltered re-export must keep UID-only moved-issue events")

	// Live-only export drops nothing here either: the subject joined no
	// local row, so there is no soft-deleted row to exclude it over.
	live := collectExport(t, restored.ExportEvents(ctx, db.ExportFilter{ProjectID: &srcP.ID}))
	require.NotNil(t, find(live, "issue.created"),
		"live-only re-export must keep UID-only moved-issue events")
}
