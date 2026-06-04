package tui

import "testing"

func TestBuildQueueRows_CollapsedShowsTopLevelOnly(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withCounts(1, 1)),
		testIssue("bbb2", withParent("aaa1")),
		testIssue("ccc3"),
	}

	rows := buildQueueRows(issues, ListFilter{}, nil)
	assertQueueShortIDs(t, rows, []string{"aaa1", "ccc3"})
	if !rows[0].hasChildren {
		t.Fatal("parent row should report children")
	}
}

func TestBuildQueueRows_ExpandedShowsDirectChildren(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withCounts(2, 2)),
		testIssue("bbb2", withParent("aaa1"), withCounts(1, 1)),
		testIssue("ccc3", withParent("aaa1")),
		testIssue("ddd4", withParent("bbb2")),
	}

	rows := buildQueueRows(issues, ListFilter{}, expansionSet{{projectID: 7, shortID: "aaa1"}: true})
	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2", "ccc3"})
	if rows[1].depth != 1 || rows[2].depth != 1 {
		t.Fatalf("children depths = %d/%d, want 1/1", rows[1].depth, rows[2].depth)
	}

	rows = buildQueueRows(issues, ListFilter{}, expansionSet{
		{projectID: 7, shortID: "aaa1"}: true,
		{projectID: 7, shortID: "bbb2"}: true,
	})
	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2", "ddd4", "ccc3"})
	if rows[2].depth != 2 {
		t.Fatalf("grandchild depth = %d, want 2", rows[2].depth)
	}
}

func TestBuildQueueRows_DefaultsExpandedChildrenToTopologicalOrder(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withCounts(3, 3)),
		testIssue("bbb2", withParent("aaa1")),
		testIssue("ccc3", withParent("aaa1"), withBlocks("bbb2")),
		testIssue("ddd4", withParent("aaa1")),
	}

	rows := buildQueueRows(issues, ListFilter{}, expansionSet{{projectID: 7, shortID: "aaa1"}: true})
	assertQueueShortIDs(t, rows, []string{"aaa1", "ccc3", "bbb2", "ddd4"})
}

func TestBuildQueueRows_TemporalChildSortPreservesFetchOrder(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withCounts(3, 3)),
		testIssue("bbb2", withParent("aaa1")),
		testIssue("ccc3", withParent("aaa1"), withBlocks("bbb2")),
		testIssue("ddd4", withParent("aaa1")),
	}

	rows := buildQueueRowsWithSort(
		issues,
		ListFilter{},
		expansionSet{{projectID: 7, shortID: "aaa1"}: true},
		childSortTemporal,
	)
	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2", "ccc3", "ddd4"})
}

func TestBuildQueueRows_FlatModePreservesFetchOrder(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withCounts(1, 1)),
		testIssue("bbb2", withParent("aaa1")),
		testIssue("ccc3"),
	}

	rows := buildQueueRowsWithView(
		issues,
		ListFilter{},
		expansionSet{{projectID: 7, shortID: "aaa1"}: true},
		childSortTopological,
		issueListViewFlat,
	)

	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2", "ccc3"})
	for i, row := range rows {
		if row.depth != 0 || row.hasChildren || row.expanded || row.context {
			t.Fatalf("flat row %d = %+v, want peer row with no tree state", i, row)
		}
	}
}

func TestBuildQueueRows_FilteredChildAutoShowsAncestorContext(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1"),
		testIssue("bbb2", withTitle("detail hint bars incomplete"), withParent("aaa1")),
		testIssue("ccc3"),
	}

	rows := buildQueueRows(issues, ListFilter{Search: "hint bars"}, nil)
	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2"})
	if !rows[0].context {
		t.Fatal("non-matching ancestor should be a context row")
	}
	if rows[1].context {
		t.Fatal("matching child should not be a context row")
	}
	if !rows[0].expanded {
		t.Fatal("ancestor context row should render as expanded while showing a matched child")
	}
}

func TestBuildQueueRows_StatusFilterIsClientSide(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withStatus("open")),
		testIssue("bbb2", withStatus("closed")),
	}

	rows := buildQueueRows(issues, ListFilter{Status: "closed"}, nil)
	assertQueueShortIDs(t, rows, []string{"bbb2"})
}

func TestBuildQueueRows_LabelsFilterAnyOf(t *testing.T) {
	issues := []Issue{
		testIssue("aaa1", withLabels("bug", "ux")),
		testIssue("bbb2", withLabels("daemon")),
	}

	rows := buildQueueRows(issues, ListFilter{Labels: []string{"ux", "docs"}}, nil)
	assertQueueShortIDs(t, rows, []string{"aaa1"})
}

func assertQueueShortIDs(t *testing.T, rows []queueRow, want []string) {
	t.Helper()
	if len(rows) != len(want) {
		t.Fatalf("rows len = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, wantSID := range want {
		if rows[i].issue.ShortID != wantSID {
			t.Fatalf("row %d = #%s, want #%s (rows=%+v)", i, rows[i].issue.ShortID, wantSID, rows)
		}
	}
}
