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

func TestBuildQueueRows_CrossProjectParentNestsByUID(t *testing.T) {
	// hub project (7) holds the real parent aaa1; project 9 holds the child
	// plus an unrelated issue reusing the short_id aaa1. The child must nest
	// under its actual parent — matched by UID — never under the
	// same-short-id issue in its own project.
	parent := testIssue("aaa1", withCounts(1, 1))
	child := testIssue("bbb2", inProject(9), withParentPeer(parent))
	impostor := testIssue("aaa1", inProject(9), withUID("01TEST-imp1"))

	rows := buildQueueRows([]Issue{parent, child, impostor}, ListFilter{},
		expansionSet{{projectID: testIssueProjectID, shortID: "aaa1"}: true})

	assertQueueShortIDs(t, rows, []string{"aaa1", "bbb2", "aaa1"})
	if rows[0].issue.ProjectID != testIssueProjectID {
		t.Fatalf("first row project = %d, want the real parent's %d", rows[0].issue.ProjectID, testIssueProjectID)
	}
	if !rows[0].hasChildren || rows[1].depth != 1 {
		t.Fatalf("child must nest under the real parent (hasChildren=%v, child depth=%d)", rows[0].hasChildren, rows[1].depth)
	}
	if rows[2].hasChildren || rows[2].depth != 0 {
		t.Fatalf("same-short-id issue in the child's project must not capture the child (hasChildren=%v, depth=%d)", rows[2].hasChildren, rows[2].depth)
	}
}

func TestBuildQueueRows_TopologicalSortResolvesBlockersByUID(t *testing.T) {
	// Siblings under one parent may now span projects, so two of them can
	// share a short_id. W blocks the project-9 vvv1; a bare short_id lookup
	// binds the edge to whichever sibling landed last in the map — here the
	// project-7 one — and orders the wrong issue after W.
	parent := testIssue("ppp1", withCounts(3, 3))
	blockedV9 := testIssue("vvv1", inProject(9), withUID("01TEST-v9"), withParentPeer(parent))
	otherV7 := testIssue("vvv1", withParentPeer(parent))
	blocker := testIssue("win1", inProject(9), withUID("01TEST-win1"),
		withParentPeer(parent), withBlocksPeer(blockedV9))

	rows := buildQueueRows([]Issue{parent, blockedV9, otherV7, blocker}, ListFilter{},
		expansionSet{{projectID: testIssueProjectID, shortID: "ppp1"}: true})

	type rowID struct {
		shortID   string
		projectID int64
	}
	got := make([]rowID, 0, len(rows))
	for _, row := range rows {
		got = append(got, rowID{row.issue.ShortID, row.issue.ProjectID})
	}
	want := []rowID{
		{"ppp1", testIssueProjectID},
		{"vvv1", testIssueProjectID}, // unconstrained sibling keeps fetch order
		{"win1", 9},                  // blocker precedes the issue it blocks
		{"vvv1", 9},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v (rows=%+v)", i, got[i], want[i], got)
		}
	}
}

func TestBuildQueueRows_TopologicalSortShortIDFallback(t *testing.T) {
	// A blocks peer without a UID (older daemon) still orders siblings when
	// its short_id is unambiguous, and is ignored when two siblings share
	// the short_id rather than guessing.
	parent := testIssue("ppp1", withCounts(2, 2))
	blocked := testIssue("aaa1", withParentPeer(parent))
	blocker := testIssue("bbb2", withParentPeer(parent))
	blocker.Blocks = []LinkPeer{{ShortID: "aaa1"}}

	rows := buildQueueRows([]Issue{parent, blocked, blocker}, ListFilter{},
		expansionSet{{projectID: testIssueProjectID, shortID: "ppp1"}: true})
	assertQueueShortIDs(t, rows, []string{"ppp1", "bbb2", "aaa1"})

	ambiguousTwin := testIssue("aaa1", inProject(9), withUID("01TEST-twin"), withParentPeer(parent))
	rows = buildQueueRows([]Issue{parent, blocked, ambiguousTwin, blocker}, ListFilter{},
		expansionSet{{projectID: testIssueProjectID, shortID: "ppp1"}: true})
	assertQueueShortIDs(t, rows, []string{"ppp1", "aaa1", "aaa1", "bbb2"})
}

func TestBuildQueueRows_CrossProjectParentAbsentRendersChildTopLevel(t *testing.T) {
	foreignParent := testIssue("zzz9", inProject(3))
	child := testIssue("bbb2", withParentPeer(foreignParent))

	rows := buildQueueRows([]Issue{child}, ListFilter{}, nil)
	assertQueueShortIDs(t, rows, []string{"bbb2"})
	if rows[0].depth != 0 {
		t.Fatalf("child of an unfetched parent must render top-level, got depth %d", rows[0].depth)
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
