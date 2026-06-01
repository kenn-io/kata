package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLabelsByIssues_EmptyInput_ReturnsEmptyMap verifies the
// short-circuit: an empty issueIDs slice returns an empty (non-nil)
// map without a SQL roundtrip. The daemon's list handler relies on
// this so an empty list page doesn't waste a query.
func TestLabelsByIssues_EmptyInput_ReturnsEmptyMap(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	got, err := d.LabelsByIssues(ctx, p.ID, nil)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = d.LabelsByIssues(ctx, p.ID, []int64{})
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

// TestLabelsByIssues_ConstrainedByProjectID verifies the cross-project
// safety check: passing an issueID belonging to project A while
// querying project B returns no labels for that ID. issue_labels has
// no project_id column, so the constraint runs through the JOIN.
func TestLabelsByIssues_ConstrainedByProjectID(t *testing.T) {
	d, ctx, pa := setupTestProject(t)
	pb := createProject(ctx, t, d, "b")
	ia := makeIssueWithLabels(t, ctx, d, pa.ID, "a", "tester", "bug")
	ib := makeIssueWithLabels(t, ctx, d, pb.ID, "b", "tester", "feature")

	// Query project A with both issue IDs; only ia's labels return.
	got, err := d.LabelsByIssues(ctx, pa.ID, []int64{ia.ID, ib.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{"bug"}, got[ia.ID])
	assert.Empty(t, got[ib.ID], "issue from a different project must not leak labels")
}

// TestLabelsByIssues_OrdersByIssueThenLabel verifies the per-issue
// alphabetical sort. Insertion order is intentionally non-alphabetical
// so the assertion would fail if ORDER BY were dropped.
func TestLabelsByIssues_OrdersByIssueThenLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	i := makeIssueWithLabels(t, ctx, d, p.ID, "a", "tester", "prio-1", "bug", "needs-review")

	got, err := d.LabelsByIssues(ctx, p.ID, []int64{i.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{"bug", "needs-review", "prio-1"}, got[i.ID])
}

// TestLabelsByIssues_MultiIssue_HappyPath verifies the map structure
// across multiple issues with overlapping and disjoint labels: each
// issue's slice is independently sorted and only contains its own labels.
func TestLabelsByIssues_MultiIssue_HappyPath(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	i1 := makeIssueWithLabels(t, ctx, d, p.ID, "a", "tester", "bug", "prio-1")
	i2 := makeIssueWithLabels(t, ctx, d, p.ID, "b", "tester", "feature", "needs-review", "bug")
	i3 := makeIssueWithLabels(t, ctx, d, p.ID, "c", "tester", "prio-1", "wontfix")

	got, err := d.LabelsByIssues(ctx, p.ID, []int64{i1.ID, i2.ID, i3.ID})
	require.NoError(t, err)
	assert.Len(t, got, 3)
	assert.Equal(t, []string{"bug", "prio-1"}, got[i1.ID])
	assert.Equal(t, []string{"bug", "feature", "needs-review"}, got[i2.ID])
	assert.Equal(t, []string{"prio-1", "wontfix"}, got[i3.ID])
}

// TestLabelsByIssues_LargeBatch_ChunksUnderSQLiteLimit verifies that the
// query stays under SQLite's default bound-parameter limit (999) when
// the caller passes more than ~1000 issue IDs. This was roborev job 246:
// a list page with limit=0 on a >999-issue project produced a 500
// because the IN clause exceeded the bound-parameter cap. The function
// now chunks the IN clause into groups of <=500 IDs and merges results.
func TestLabelsByIssues_LargeBatch_ChunksUnderSQLiteLimit(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	const n = 1500
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		issue := makeIssueWithLabels(t, ctx, d, p.ID, "i", "tester", "bug")
		ids = append(ids, issue.ID)
	}

	got, err := d.LabelsByIssues(ctx, p.ID, ids)
	require.NoError(t, err, "1500-issue batch must not exceed SQLite parameter limit")
	assert.Len(t, got, n, "every issue's labels must be returned, regardless of chunking")
	for _, id := range ids {
		assert.Equal(t, []string{"bug"}, got[id])
	}
}

// TestLabelsByIssues_IssueWithNoLabelsAbsent verifies the contract that
// issues with no labels are absent from the map. Callers treat a
// missing key as "no labels"; this prevents allocation noise on the
// common case where most issues are unlabeled.
func TestLabelsByIssues_IssueWithNoLabelsAbsent(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	i1 := makeIssueWithLabels(t, ctx, d, p.ID, "labeled", "tester", "bug")
	i2 := makeIssue(t, ctx, d, p.ID, "naked", "tester")

	got, err := d.LabelsByIssues(ctx, p.ID, []int64{i1.ID, i2.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{"bug"}, got[i1.ID])
	_, present := got[i2.ID]
	assert.False(t, present, "issue with no labels must be absent from map")
}
