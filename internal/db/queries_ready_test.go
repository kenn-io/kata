package db_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestReadyIssues_FiltersOutClosed(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	open := makeIssue(t, ctx, d, p.ID, "open", "tester")
	closed := makeIssue(t, ctx, d, p.ID, "closed", "tester")
	_, _, _, err := d.CloseIssue(ctx, closed.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	got := readyNumbers(t, ctx, d, p.ID)
	assert.Contains(t, got, open.ShortID)
	assert.NotContains(t, got, closed.ShortID)
}

func TestReadyIssues_ExcludesIssuesBlockedByOpenBlocker(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	blocker := makeIssue(t, ctx, d, p.ID, "blocker", "tester")
	blocked := makeIssue(t, ctx, d, p.ID, "blocked", "tester")
	standalone := makeIssue(t, ctx, d, p.ID, "standalone", "tester")
	makeLink(ctx, t, d, p.ID, blocker.ID, blocked.ID, "blocks")

	got := readyNumbers(t, ctx, d, p.ID)
	assert.Contains(t, got, blocker.ShortID, "blocker is ready (not blocked itself)")
	assert.Contains(t, got, standalone.ShortID, "standalone is ready")
	assert.NotContains(t, got, blocked.ShortID, "blocked is not ready while blocker is open")
}

func TestReadyIssues_ClosedBlockerUnblocksDownstream(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	blocker := makeIssue(t, ctx, d, p.ID, "blocker", "tester")
	blocked := makeIssue(t, ctx, d, p.ID, "blocked", "tester")
	makeLink(ctx, t, d, p.ID, blocker.ID, blocked.ID, "blocks")
	_, _, _, err := d.CloseIssue(ctx, blocker.ID, "done", "tester", "", nil)
	require.NoError(t, err)

	got := readyNumbers(t, ctx, d, p.ID)
	assert.Contains(t, got, blocked.ShortID, "blocked is ready once blocker closes")
}

func TestReadyIssues_FilterByUnowned(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	unowned := makeIssue(t, ctx, d, p.ID, "unowned", "tester")
	owned := makeIssue(t, ctx, d, p.ID, "owned", "tester")
	owner := "alice"
	_, _, _, err := d.UpdateOwner(ctx, owned.ID, &owner, "tester")
	require.NoError(t, err)

	// Filter for unowned issues only
	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{Unowned: true})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, unowned.ShortID)
	assert.NotContains(t, got, owned.ShortID)
}

func TestReadyIssues_FilterByOwner(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	aliceIssue := makeIssue(t, ctx, d, p.ID, "alice task", "tester")
	bobIssue := makeIssue(t, ctx, d, p.ID, "bob task", "tester")
	unowned := makeIssue(t, ctx, d, p.ID, "unowned", "tester")

	alice := "alice"
	bob := "bob"
	_, _, _, err := d.UpdateOwner(ctx, aliceIssue.ID, &alice, "tester")
	require.NoError(t, err)
	_, _, _, err = d.UpdateOwner(ctx, bobIssue.ID, &bob, "tester")
	require.NoError(t, err)

	// Filter for alice's issues only
	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{Owner: "alice"})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, aliceIssue.ShortID)
	assert.NotContains(t, got, bobIssue.ShortID)
	assert.NotContains(t, got, unowned.ShortID)
}

func TestReadyIssues_FilterByLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	bug := makeIssueWithLabels(t, ctx, d, p.ID, "bug issue", "tester", "bug")
	feature := makeIssueWithLabels(t, ctx, d, p.ID, "feature issue", "tester", "feature")
	bugAndP0 := makeIssueWithLabels(t, ctx, d, p.ID, "urgent bug", "tester", "bug", "p0")
	noLabels := makeIssue(t, ctx, d, p.ID, "no labels", "tester")

	// Filter for issues with "bug" label
	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{Labels: []string{"bug"}})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, bug.ShortID)
	assert.Contains(t, got, bugAndP0.ShortID, "should include issues with bug AND other labels")
	assert.NotContains(t, got, feature.ShortID)
	assert.NotContains(t, got, noLabels.ShortID)

	// Filter for issues with BOTH "bug" AND "p0" labels (AND logic)
	rows, err = d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{Labels: []string{"bug", "p0"}})
	require.NoError(t, err)
	got = make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, bugAndP0.ShortID)
	assert.NotContains(t, got, bug.ShortID, "should not include issues with only bug label")
	assert.NotContains(t, got, feature.ShortID)
}

func TestReadyIssues_LabelFiltersAreCaseInsensitive(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	bug := makeIssueWithLabels(t, ctx, d, p.ID, "bug issue", "tester", "bug")
	feature := makeIssueWithLabels(t, ctx, d, p.ID, "feature issue", "tester", "feature")

	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{
		Labels:        []string{"Bug"},
		ExcludeLabels: []string{"Feature"},
	})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, bug.ShortID)
	assert.NotContains(t, got, feature.ShortID)
}

func TestReadyIssues_FilterByNoLabel(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	bug := makeIssueWithLabels(t, ctx, d, p.ID, "bug issue", "tester", "bug")
	wontfix := makeIssueWithLabels(t, ctx, d, p.ID, "wontfix issue", "tester", "wontfix")
	noLabels := makeIssue(t, ctx, d, p.ID, "clean issue", "tester")

	// Exclude issues with "bug" label
	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{ExcludeLabels: []string{"bug"}})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.NotContains(t, got, bug.ShortID)
	assert.Contains(t, got, wontfix.ShortID)
	assert.Contains(t, got, noLabels.ShortID)

	// Exclude issues with "bug" OR "wontfix" labels
	rows, err = d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{ExcludeLabels: []string{"bug", "wontfix"}})
	require.NoError(t, err)
	got = make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.NotContains(t, got, bug.ShortID)
	assert.NotContains(t, got, wontfix.ShortID)
	assert.Contains(t, got, noLabels.ShortID)
}

func TestReadyIssues_FilterComposition(t *testing.T) {
	d, ctx, p := setupTestProject(t)

	// Create various issues to test filter composition
	alicesBug := makeIssueWithLabels(t, ctx, d, p.ID, "alice bug", "tester", "bug")
	alice := "alice"
	_, _, _, err := d.UpdateOwner(ctx, alicesBug.ID, &alice, "tester")
	require.NoError(t, err)

	bobsBug := makeIssueWithLabels(t, ctx, d, p.ID, "bob bug", "tester", "bug")
	bob := "bob"
	_, _, _, err = d.UpdateOwner(ctx, bobsBug.ID, &bob, "tester")
	require.NoError(t, err)

	unownedBug := makeIssueWithLabels(t, ctx, d, p.ID, "unowned bug", "tester", "bug")
	unownedWontfix := makeIssueWithLabels(t, ctx, d, p.ID, "unowned wontfix", "tester", "wontfix")
	unownedClean := makeIssue(t, ctx, d, p.ID, "unowned clean", "tester")

	// Test: unowned + has bug label + no wontfix label
	rows, err := d.ReadyIssues(ctx, p.ID, 0, db.ReadyIssuesFilter{
		Unowned:       true,
		Labels:        []string{"bug"},
		ExcludeLabels: []string{"wontfix"},
	})
	require.NoError(t, err)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.ShortID)
	}

	assert.Contains(t, got, unownedBug.ShortID, "unowned bug with no wontfix label should be included")
	assert.NotContains(t, got, alicesBug.ShortID, "alice's bug has owner")
	assert.NotContains(t, got, bobsBug.ShortID, "bob's bug has owner")
	assert.NotContains(t, got, unownedWontfix.ShortID, "has wontfix label")
	assert.NotContains(t, got, unownedClean.ShortID, "no bug label")
}

// readyNumbers fetches ready issues for projectID and returns their short IDs.
//
//nolint:revive // test helper: t *testing.T conventionally precedes ctx.
func readyNumbers(t *testing.T, ctx context.Context, d *db.DB, projectID int64) []string {
	t.Helper()
	rows, err := d.ReadyIssues(ctx, projectID, 0, db.ReadyIssuesFilter{})
	require.NoError(t, err)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ShortID)
	}
	return out
}

func TestReadyIssuesGlobal_ReturnsIssuesAcrossProjects(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2, err := d.CreateProject(ctx, "second-project")
	require.NoError(t, err)

	a := makeIssue(t, ctx, d, p1.ID, "in p1", "tester")
	b := makeIssue(t, ctx, d, p2.ID, "in p2", "tester")

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)

	got := map[string]string{}
	for _, r := range rows {
		got[r.ShortID] = r.ProjectName
	}
	assert.Equal(t, p1.Name, got[a.ShortID])
	assert.Equal(t, "second-project", got[b.ShortID])
}

func TestReadyIssuesGlobal_ExcludesArchivedProjects(t *testing.T) {
	d, ctx, p1 := setupTestProject(t)
	p2, err := d.CreateProject(ctx, "to-archive")
	require.NoError(t, err)

	keep := makeIssue(t, ctx, d, p1.ID, "keep", "tester")
	hidden := makeIssue(t, ctx, d, p2.ID, "hidden", "tester")

	_, _, err = d.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: p2.ID,
		Actor:     "tester",
		Force:     true,
	})
	require.NoError(t, err)

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, r := range rows {
		got[r.ShortID] = true
	}
	assert.True(t, got[keep.ShortID], "issue in active project is returned")
	assert.False(t, got[hidden.ShortID], "issue in archived project is excluded")
}

func TestReadyIssuesGlobal_ExcludesBlockedIssues(t *testing.T) {
	d, ctx, p := setupTestProject(t)
	blocker := makeIssue(t, ctx, d, p.ID, "blocker", "tester")
	blocked := makeIssue(t, ctx, d, p.ID, "blocked", "tester")
	makeLink(ctx, t, d, p.ID, blocker.ID, blocked.ID, "blocks")

	rows, err := d.ReadyIssuesGlobal(ctx, 0)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.ShortID] = true
	}
	assert.True(t, got[blocker.ShortID])
	assert.False(t, got[blocked.ShortID])
}
