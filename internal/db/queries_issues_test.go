package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestCreateIssue_AllocatesShortIDAndEmitsEvent(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	issue, evt, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		Title:     "first",
		Body:      "details",
		Author:    "agent-1",
	})
	require.NoError(t, err)
	assertValidUID(t, issue.UID)
	assert.NotEmpty(t, issue.ShortID)
	assert.Equal(t, p.UID, issue.ProjectUID)
	assert.Equal(t, "open", issue.Status)
	assert.Equal(t, "agent-1", issue.Author)
	assert.Equal(t, "issue.created", evt.Type)
	assert.Equal(t, p.UID, evt.ProjectUID)
	assert.NotNil(t, evt.IssueID)
	require.NotNil(t, evt.IssueUID)
	assert.Equal(t, issue.UID, *evt.IssueUID)
}

func TestCreateIssue_ShortIDsAreUniquePerProject(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	seen := map[string]struct{}{}
	for i := 1; i <= 3; i++ {
		issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: p.ID, Title: "x", Author: "a",
		})
		require.NoError(t, err)
		_, dup := seen[issue.ShortID]
		assert.False(t, dup, "short_id %q should be unique within the project", issue.ShortID)
		seen[issue.ShortID] = struct{}{}
	}
}

// TestCreateIssue_ShortIDOverridePersistsVerbatim pins spec §8.1: JSONL
// import passes the stored short_id and CreateIssue uses it as-is, bypassing
// auto-extend. The override must be the lowercased suffix of UID at its
// length — anything else is a caller bug that returns an error.
func TestCreateIssue_ShortIDOverridePersistsVerbatim(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	issue, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       p.ID,
		UID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ShortIDOverride: "bcd4ex",
		Title:           "preserved",
		Author:          "tester",
	})
	require.NoError(t, err)
	assert.Equal(t, "bcd4ex", issue.ShortID)
}

func TestCreateIssue_ShortIDOverrideRejectsMismatch(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       p.ID,
		UID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ShortIDOverride: "abcd", // does not match the suffix of UID at length 4
		Title:           "bad",
		Author:          "tester",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

func TestCreateIssue_ShortIDOverrideRejectsInvalidSyntax(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	_, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID:       p.ID,
		UID:             "01HZNQ7VFPK1XGD8R5MABCD4EX",
		ShortIDOverride: "AB", // too short, also uppercase
		Title:           "bad",
		Author:          "tester",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid short_id override")
}

// TestIssueByShortID_ReturnsLiveIssue pins that a live issue resolves by its
// stored short_id under the default include-deleted=no filter.
func TestIssueByShortID_ReturnsLiveIssue(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "find me",
		Author:    "tester",
	})
	require.NoError(t, err)

	got, err := d.IssueByShortID(ctx, p.ID, "d4ex", db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Equal(t, created.UID, got.UID)
}

// TestIssueByShortID_NotFoundForUnknownShortID pins that a short_id with no
// matching row returns ErrNotFound rather than a zero-value Issue.
func TestIssueByShortID_NotFoundForUnknownShortID(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	_, err := d.IssueByShortID(ctx, p.ID, "zzzz", db.IncludeDeletedNo)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// TestIssueByShortID_DefaultExcludesSoftDeleted pins spec §6: normal read
// paths hide soft-deleted rows. The same short_id that resolved before
// SoftDeleteIssue must return ErrNotFound after.
func TestIssueByShortID_DefaultExcludesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "soon-gone",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, created.ID, "tester")
	require.NoError(t, err)

	_, err = d.IssueByShortID(ctx, p.ID, created.ShortID, db.IncludeDeletedNo)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

// TestIssueByShortID_IncludeDeletedYesResolvesSoftDeleted pins the carveout
// branch (spec §6): restore/delete/purge/idempotency-collision pass
// IncludeDeletedYes and must see the soft-deleted row.
func TestIssueByShortID_IncludeDeletedYesResolvesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "soon-gone",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, created.ID, "tester")
	require.NoError(t, err)

	got, err := d.IssueByShortID(ctx, p.ID, created.ShortID, db.IncludeDeletedYes)
	require.NoError(t, err)
	assert.Equal(t, created.UID, got.UID)
}

// TestIssueUIDPrefixMatch_DefaultExcludesSoftDeleted pins that the default
// include-deleted=no filter mirrors IssueByUID — a soft-deleted issue must
// not surface through the prefix-match fallback used by the daemon's global
// /api/v1/issues/{ref} handler.
func TestIssueUIDPrefixMatch_DefaultExcludesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "soon-gone",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, created.ID, "tester")
	require.NoError(t, err)

	matches, err := d.IssueUIDPrefixMatch(ctx, "01HZNQ7V", 20, db.IncludeDeletedNo)
	require.NoError(t, err)
	assert.Empty(t, matches)
}

// TestIssueUIDPrefixMatch_IncludeDeletedYesResolvesSoftDeleted pins the
// carveout branch (spec §6): callers passing IncludeDeletedYes (restore,
// idempotent re-delete) must see soft-deleted rows when matched by UID
// prefix.
func TestIssueUIDPrefixMatch_IncludeDeletedYesResolvesSoftDeleted(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()
	p := createProject(ctx, t, d, "demo")
	created, _, err := d.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: p.ID,
		UID:       "01HZNQ7VFPK1XGD8R5MABCD4EX",
		Title:     "soon-gone",
		Author:    "tester",
	})
	require.NoError(t, err)
	_, _, _, err = d.SoftDeleteIssue(ctx, created.ID, "tester")
	require.NoError(t, err)

	matches, err := d.IssueUIDPrefixMatch(ctx, "01HZNQ7V", 20, db.IncludeDeletedYes)
	require.NoError(t, err)
	require.Len(t, matches, 1)
	assert.Equal(t, created.UID, matches[0].UID)
}

func TestListIssues_DefaultsToOpenOnlyAndExcludesDeleted(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	for _, title := range []string{"a", "b", "c"} {
		createTesterIssue(ctx, t, d, p.ID, title)
	}

	got, err := d.ListIssues(ctx, db.ListIssuesParams{ProjectID: p.ID, Status: "open"})
	require.NoError(t, err)
	assert.Len(t, got, 3)
}

func TestListIssues_LabelFiltersAreCaseInsensitive(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	bug := makeIssueWithLabels(t, ctx, d, p.ID, "bug issue", "tester", "bug")
	feature := makeIssueWithLabels(t, ctx, d, p.ID, "feature issue", "tester", "feature")

	got, err := d.ListIssues(ctx, db.ListIssuesParams{
		ProjectID:     p.ID,
		Labels:        []string{"Bug"},
		ExcludeLabels: []string{"Feature"},
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, bug.ShortID, got[0].ShortID)
	assert.NotEqual(t, feature.ShortID, got[0].ShortID)
}

// TestListAllIssues_CoversAllProjectsAndOrders pins #22's contract: with
// ProjectID==0 every project's issues are returned, soft-deleted rows are
// excluded, and the ordering is created_at DESC, id DESC.
func TestListAllIssues_CoversAllProjectsAndOrders(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2 := createProject(ctx, t, d, "beta")

	a1, _ := createTesterIssue(ctx, t, d, p1.ID, "alpha-1")
	b1, _ := createTesterIssue(ctx, t, d, p2.ID, "beta-1")
	a2, _ := createTesterIssue(ctx, t, d, p1.ID, "alpha-2")

	got, err := d.ListAllIssues(ctx, db.ListAllIssuesParams{})
	require.NoError(t, err)
	require.Len(t, got, 3)
	// created_at DESC, id DESC: latest insert comes first. Insert order was
	// a1, b1, a2 — id-DESC tiebreak guarantees stable ordering even when
	// several issues share a created_at sub-second bucket.
	assert.Equal(t, a2.ID, got[0].ID)
	assert.Equal(t, b1.ID, got[1].ID)
	assert.Equal(t, a1.ID, got[2].ID)
}

// TestListAllIssues_ProjectFilterScopes pins the optional project_id query:
// passing ProjectID>0 returns only that project's issues.
func TestListAllIssues_ProjectFilterScopes(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2 := createProject(ctx, t, d, "beta")
	createTesterIssue(ctx, t, d, p1.ID, "a1")
	createTesterIssue(ctx, t, d, p2.ID, "b1")

	got, err := d.ListAllIssues(ctx, db.ListAllIssuesParams{ProjectID: p2.ID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, p2.ID, got[0].ProjectID)
}

// TestListAllIssues_StatusFilterApplies pins the status filter across
// projects: closed/open are honored.
func TestListAllIssues_StatusFilterApplies(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	open1, _ := createTesterIssue(ctx, t, d, p.ID, "open")
	closed1, _ := createTesterIssue(ctx, t, d, p.ID, "to-close")
	_, _, _, err := d.CloseIssue(ctx, closed1.ID, "done", "x", "", nil)
	require.NoError(t, err)

	got, err := d.ListAllIssues(ctx, db.ListAllIssuesParams{Status: "open"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, open1.ID, got[0].ID)
}

// TestListAllIssues_ExcludesSoftDeleted pins that purged/soft-deleted issues
// don't surface in the cross-project list.
func TestListAllIssues_ExcludesSoftDeleted(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	live, _ := createTesterIssue(ctx, t, d, p.ID, "live")
	doomed, _ := createTesterIssue(ctx, t, d, p.ID, "doomed")
	_, _, _, err := d.SoftDeleteIssue(ctx, doomed.ID, "x")
	require.NoError(t, err)

	got, err := d.ListAllIssues(ctx, db.ListAllIssuesParams{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, live.ID, got[0].ID)
}

// TestListAllIssues_LimitCaps pins the limit knob on cross-project listing.
func TestListAllIssues_LimitCaps(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	createTesterIssues(ctx, t, d, p.ID, 5)

	got, err := d.ListAllIssues(ctx, db.ListAllIssuesParams{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestCreateComment_EmitsEvent(t *testing.T) {
	d, ctx, p, issue := setupTestIssue(t)

	cmt, evt, err := d.CreateComment(ctx, db.CreateCommentParams{
		IssueID: issue.ID, Author: "agent", Body: "hi",
	})
	require.NoError(t, err)
	assert.Equal(t, "hi", cmt.Body)
	assert.Equal(t, "issue.commented", evt.Type)
	assert.Equal(t, p.UID, evt.ProjectUID)
	require.NotNil(t, evt.IssueUID)
	assert.Equal(t, issue.UID, *evt.IssueUID)
}

func TestCloseIssue_SetsStatusAndEmitsEvent(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	updated, evt, changed, err := d.CloseIssue(ctx, issue.ID, "done", "agent", "", nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "closed", updated.Status)
	require.NotNil(t, updated.ClosedReason)
	assert.Equal(t, "done", *updated.ClosedReason)
	assert.NotNil(t, updated.ClosedAt)
	assert.Equal(t, "issue.closed", evt.Type)
}

func TestCloseIssue_OnAlreadyClosedIsNoOp(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)
	_, _, _, err := d.CloseIssue(ctx, issue.ID, "done", "agent", "", nil)
	require.NoError(t, err)

	_, evt, changed, err := d.CloseIssue(ctx, issue.ID, "done", "agent", "", nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, evt)
}

func TestReopenIssue_ClearsStatusAndEmitsEvent(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)
	_, _, _, err := d.CloseIssue(ctx, issue.ID, "done", "agent", "", nil)
	require.NoError(t, err)

	updated, evt, changed, err := d.ReopenIssue(ctx, issue.ID, "agent")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "open", updated.Status)
	assert.Nil(t, updated.ClosedAt)
	assert.Nil(t, updated.ClosedReason)
	assert.Equal(t, "issue.reopened", evt.Type)
}

func TestEditIssue_SetsFieldsAndEmitsEvent(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	newTitle := "new"
	updated, evt, changed, err := d.EditIssue(ctx, db.EditIssueParams{
		IssueID: issue.ID, Title: &newTitle, Actor: "agent",
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "new", updated.Title)
	require.NotNil(t, evt)
	assert.Equal(t, "issue.updated", evt.Type)
}

func TestEditIssue_NoFieldsIsValidationError(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	_, _, _, err := d.EditIssue(ctx, db.EditIssueParams{IssueID: issue.ID, Actor: "agent"})
	assert.ErrorIs(t, err, db.ErrNoFields)
}

// TestCloseIssue_EmptyReasonRejected pins that db.CloseIssue refuses an
// empty reason rather than silently coercing it to "done". The handler
// owns reason defaulting (for the TUI bypass path) so the db layer's
// surprise default is gone; this regression test catches any reintroduction.
func TestCloseIssue_EmptyReasonRejected(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	_, _, _, err := d.CloseIssue(ctx, issue.ID, "", "wesm", "", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reason is required")
}

func TestCloseIssue_SupersededReasonAccepted(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	_, _, _, err := d.CloseIssue(ctx, issue.ID, "superseded", "wesm", "", nil)
	require.NoError(t, err)
}

func TestCloseIssue_AuditNoChangeReasonAccepted(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	_, _, _, err := d.CloseIssue(ctx, issue.ID, "audit-no-change", "wesm", "", nil)
	require.NoError(t, err)
}

func TestCloseIssue_RefusesParentWithOpenChildren(t *testing.T) {
	// The in-transaction guard mirrors CheckParentCloseCompleteness in the
	// daemon handler so a child link inserted between the read-side check
	// and the close write still aborts the close at commit time.
	d, ctx, p, parent := setupTestIssue(t)
	child, _ := createTesterIssue(ctx, t, d, p.ID, "child")
	makeLink(ctx, t, d, p.ID, child.ID, parent.ID, "parent")

	_, _, _, err := d.CloseIssue(ctx, parent.ID, "done", "agent", "", nil)
	require.ErrorIs(t, err, db.ErrOpenChildren)
}

func TestCloseIssue_AllowsParentWithOnlyClosedChildren(t *testing.T) {
	d, ctx, p, parent := setupTestIssue(t)
	child, _ := createTesterIssue(ctx, t, d, p.ID, "child")
	makeLink(ctx, t, d, p.ID, child.ID, parent.ID, "parent")
	_, _, _, err := d.CloseIssue(ctx, child.ID, "done", "agent", "", nil)
	require.NoError(t, err)

	_, _, _, err = d.CloseIssue(ctx, parent.ID, "done", "agent", "", nil)
	require.NoError(t, err)
}

func TestCloseIssue_PersistsMessageAndEvidence(t *testing.T) {
	d, ctx, _, issue := setupTestIssue(t)

	evidence := []db.Evidence{{Type: "commit", SHA: "abc1234"}}
	_, evt, _, err := d.CloseIssue(ctx, issue.ID, "done", "wesm",
		"Fixed the bug and ran tests.", evidence)
	require.NoError(t, err)
	require.NotNil(t, evt)

	// Payload should contain reason, message, and evidence.
	assert.Contains(t, evt.Payload, `"reason":"done"`)
	assert.Contains(t, evt.Payload, `"message":"Fixed the bug and ran tests."`)
	assert.Contains(t, evt.Payload, `"evidence":[`)
	assert.Contains(t, evt.Payload, `"type":"commit"`)
	assert.Contains(t, evt.Payload, `"sha":"abc1234"`)
}
