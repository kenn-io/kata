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
	a, err := d.AttachAlias(ctx, p.ID, "alias-x", "git", "/tmp/x")
	require.NoError(t, err)

	got := collectExport(t, d.ExportProjectAliases(ctx, db.ExportFilter{ProjectID: &p.ID}))
	require.Len(t, got, 1)
	require.Equal(t, a.ID, got[0].ID)
	require.Equal(t, p.ID, got[0].ProjectID)
	require.Equal(t, "/tmp/x", got[0].RootPath)
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
	_, err = d.CreateLink(ctx, db.CreateLinkParams{ProjectID: p.ID, FromIssueID: a.ID, ToIssueID: b.ID, Type: "blocks", Author: "x"})
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
