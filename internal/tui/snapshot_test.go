package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateGoldens enables `go test ./internal/tui/ -update-goldens` so the
// snapshot tests rewrite their .txt fixtures rather than asserting. We
// use a custom name (not -update) so a future go test flag of that name
// would not collide with this one. The flag is package-level because
// every snapshot test reads it through assertGolden.
var updateGoldens = flag.Bool(
	"update-goldens", false, "update golden snapshot files",
)

// snapshotFixedNow is the wall-clock the snapshot tests freeze
// renderNow to so the "Nh ago" column stays deterministic across runs.
// Chosen to be deep in the future so any "X ago" deltas computed
// against fixture timestamps are stable. Tests that depend on the
// "ago" math compute fixture timestamps relative to this constant.
var snapshotFixedNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// snapshotInit prepares the package for a deterministic snapshot run:
//   - KATA_COLOR_MODE=none strips ANSI from rendered output, so the
//     golden files are plain UTF-8 text with no escape sequences.
//   - applyDefaultColorMode rebuilds the package-level styles against
//     io.Discard with the just-set env var so the new mode takes effect.
//   - renderNow is frozen at snapshotFixedNow; the caller restores the
//     prior renderNow when the test ends so other tests see time.Now.
//
// Returns a cleanup func; tests defer it.
func snapshotInit(t *testing.T) func() {
	t.Helper()
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	prior := renderNow
	renderNow = func() time.Time { return snapshotFixedNow }
	return func() {
		renderNow = prior
		applyDefaultColorMode()
	}
}

// assertGolden compares got against testdata/golden/<name>.txt. With
// -update-goldens, the file is (re)written with got. Without it, a
// missing file is a hard failure with a hint to regenerate; a mismatch
// shows both want and got verbatim so the diff is reviewable. The
// gosec hints (G301/G304/G306) are expected here: the test owns the
// path under testdata/golden/, the file is read-only fixture data, and
// 0o750/0o600 are sufficient for fixtures committed to the repo.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	got = normalizeGoldenSnapshot(got)
	path := filepath.Join("testdata", "golden", name+".txt")
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is under testdata/golden/
	if err != nil {
		t.Fatalf(
			"missing golden %q: %v (run `go test ./internal/tui/ "+
				"-update-goldens` to create)", name, err,
		)
	}
	if got != normalizeGoldenSnapshot(string(want)) {
		t.Errorf(
			"%s golden mismatch:\n--- want ---\n%s\n--- got ---\n%s",
			name, normalizeGoldenSnapshot(string(want)), got,
		)
	}
}

func normalizeGoldenSnapshot(s string) string {
	// Strip ANSI first: Lip Gloss v2 styles always emit attribute
	// sequences (bold/faint/reverse) — profile downsampling moved from
	// style-build time into Bubble Tea's compositor — so colorNone
	// output is no longer escape-free by construction. Goldens stay
	// plain UTF-8 text.
	s = stripANSI(s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.Join(lines, "\n")
}

// snapViewChrome returns the steady-state viewChrome used by every
// list snapshot: project 7 / "kata" scope, SSE connected, version
// "v0.1.0". Tests that need to mutate (e.g. add an input bar) assign
// to a local and modify the field they care about.
func snapViewChrome() viewChrome {
	return viewChrome{
		scope:     scope{projectID: 7, projectName: "kata"},
		sseStatus: sseConnected,
		version:   "v0.1.0",
	}
}

// snapListModel returns a listModel pre-loaded with the given issues
// and loading flipped off — the universal starting point for every
// TestSnapshot_List_* fixture.
func snapListModel(issues []Issue) listModel {
	lm := newListModel()
	lm.loading = false
	lm.issues = issues
	return lm
}

// snapListFixture mirrors listFixture but pegs every UpdatedAt at a
// known offset from snapshotFixedNow so humanizeRelative renders
// deterministic deltas. The deleted row also has a fixed DeletedAt.
func snapListFixture() []Issue {
	return []Issue{
		{
			UID: "01TEST-aaa1", ShortID: "aaa1", Title: "fix login bug on Safari",
			Status: "open", Owner: ptrString("claude-4.7"),
			Priority:  ptrInt64(1),
			UpdatedAt: snapshotFixedNow.Add(-3 * time.Hour),
		},
		{
			UID: "01TEST-bbb2", ShortID: "bbb2", Title: "rebuild search index",
			Status: "closed", Owner: ptrString("wesm"),
			UpdatedAt: snapshotFixedNow.Add(-1 * time.Hour),
		},
		{
			UID: "01TEST-ccc3", ShortID: "ccc3", Title: "purge stale tokens",
			Status:    "open",
			DeletedAt: ptrTime(snapshotFixedNow.Add(-2 * time.Hour)),
			UpdatedAt: snapshotFixedNow.Add(-2 * time.Hour),
		},
	}
}

func snapTreeFixture() []Issue {
	parentSID := "p010"
	childSID := "c011"
	return []Issue{
		{
			ProjectID: 7, UID: "01TEST-p010", ShortID: parentSID, Title: "professional workspace polish",
			Status: "open", Owner: ptrString("wesm"),
			UpdatedAt:   snapshotFixedNow.Add(-30 * time.Minute),
			ChildCounts: &ChildCounts{Open: 1, Total: 2},
		},
		{
			ProjectID: 7, UID: "01TEST-c011", ShortID: childSID, Parent: &LinkPeer{UID: "01TEST-" + parentSID, ShortID: parentSID},
			Title: "detail hint bars incomplete", Status: "open",
			Owner:       ptrString("claude"),
			UpdatedAt:   snapshotFixedNow.Add(-45 * time.Minute),
			ChildCounts: &ChildCounts{Open: 1, Total: 1},
		},
		{
			ProjectID: 7, UID: "01TEST-c012", ShortID: "c012", Parent: &LinkPeer{UID: "01TEST-" + parentSID, ShortID: parentSID},
			Title: "new issue form parent field", Status: "closed",
			UpdatedAt: snapshotFixedNow.Add(-2 * time.Hour),
		},
		{
			ProjectID: 7, UID: "01TEST-g013", ShortID: "g013", Parent: &LinkPeer{UID: "01TEST-" + childSID, ShortID: childSID},
			Title: "child detail jump target", Status: "open",
			UpdatedAt: snapshotFixedNow.Add(-3 * time.Hour),
		},
		{
			ProjectID: 7, UID: "01TEST-s020", ShortID: "s020", Title: "standalone queue cleanup",
			Status: "open", UpdatedAt: snapshotFixedNow.Add(-4 * time.Hour),
		},
	}
}

// snapDetailFixture builds a detailModel with two comments, two events,
// and one link so each tab snapshot has data to render. All timestamps
// are absolute so fmtTime produces the same output every run.
func snapDetailFixture() detailModel {
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	iss := Issue{
		ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "fix login bug on Safari",
		Status: "open", Author: "wesm",
		Body: "Reproduces in Safari 17 only.\nClick the login button twice.",
	}
	return detailModel{
		issue:    &iss,
		scopePID: 7,
		comments: []CommentEntry{
			{ID: 1, Author: "alice", Body: "I can repro on macOS.", CreatedAt: when},
			{ID: 2, Author: "bob", Body: "Looks like a race in oauth.", CreatedAt: when.Add(time.Hour)},
		},
		events: []EventLogEntry{
			{ID: 9, Type: "issue.created", Actor: "alice", CreatedAt: when},
			{ID: 10, Type: "issue.commented", Actor: "bob", CreatedAt: when.Add(time.Hour)},
		},
		links: []LinkEntry{
			{
				ID: 100, Type: "blocks",
				From:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
				To:     LinkPeer{UID: "01TEST-7bb", ShortID: "7bb"},
				Author: "wesm", CreatedAt: when,
			},
		},
	}
}

func snapDetailHierarchyFixture() detailModel {
	dm := snapDetailFixture()
	dm.parent = &IssueRef{UID: "01TEST-c012", ShortID: "c012", Title: "workspace polish parent", Status: "open"}
	dm.children = []Issue{
		{
			ProjectID: 7, UID: "01TEST-c043", ShortID: "c043", Title: "detail hint bars incomplete",
			Status: "open", Owner: ptrString("alice"),
			UpdatedAt: snapshotFixedNow.Add(-1 * time.Hour),
		},
		{
			ProjectID: 7, UID: "01TEST-c044", ShortID: "c044", Title: "new issue form parent field",
			Status: "closed", Owner: ptrString("wesm"),
			UpdatedAt: snapshotFixedNow.Add(-2 * time.Hour),
		},
	}
	return dm
}

// TestSnapshot_List_DefaultMixedStatus locks down the steady-state list
// view at width 120 with three rows and the cursor on row 2. The fixture
// covers open, closed, and soft-deleted statusChip branches.
func TestSnapshot_List_DefaultMixedStatus(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapListFixture())
	lm.cursor = 1
	got := lm.View(120, 30, snapViewChrome())
	assertGolden(t, "list-default-mixed-status", got)
}

// All snapshot tests run under KATA_COLOR_MODE=none for deterministic
// goldens (the renderer writes through io.Discard, which is not a TTY,
// so lipgloss strips ANSI even in colored modes). The plan called for a
// separate list-no-color-mode snapshot, but it would be byte-identical
// to list-default-mixed-status under the harness above and add no
// coverage. ANSI suppression is verified directly by theme_test.go's
// TestApplyColorMode_NoneStripsForeground; the snapshot suite locks
// down rendering shape, not color escape semantics.

// TestSnapshot_List_EmptyAfterFilter exercises the "no rows visible"
// branch of renderBody: a search filter narrows everything out so the
// empty-state hint appears.
func TestSnapshot_List_EmptyAfterFilter(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapListFixture())
	lm.filter = ListFilter{Search: "no-match-anywhere"}
	got := lm.View(120, 30, snapViewChrome())
	assertGolden(t, "list-empty-after-filter", got)
}

// TestSnapshot_QuitConfirmModal covers the M3.5b quit-confirm
// modal overlaid on a list view. The modal sits centered over the
// rendered background; underlying content stays painted around it.
func TestSnapshot_QuitConfirmModal(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapListFixture())
	lm.cursor = 1
	bg := lm.View(120, 30, snapViewChrome())
	got := overlayModal(bg, renderQuitConfirmModal(), 120, 30)
	assertGolden(t, "quit-confirm-modal", got)
}

// TestSnapshot_List_SearchBarActive covers the inline command bar
// in place of the chip strip when chrome.input.kind == inputSearchBar.
// The footer help row swaps to the bar's enter/esc/ctrl+u keys.
func TestSnapshot_List_SearchBarActive(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapListFixture())
	chrome := snapViewChrome()
	chrome.input = newSearchBar(ListFilter{Search: "login"})
	got := lm.View(120, 30, chrome)
	assertGolden(t, "list-search-bar-active", got)
}

// TestSnapshot_List_ScrollIndicator covers the scroll-indicator slot
// in the footer status line. With 50 issues and a 30-row terminal, the
// chrome reserves enough rows that not every issue fits — the
// indicator surfaces as `[start-end of N issues]` aligned right.
func TestSnapshot_List_ScrollIndicator(t *testing.T) {
	defer snapshotInit(t)()
	issues := make([]Issue, 50)
	for i := range issues {
		sid := fmt.Sprintf("s%03d", i+1)
		issues[i] = Issue{
			UID:     "01TEST-" + sid,
			ShortID: sid,
			Title:   "issue " + sid,
			Status:  "open",
			UpdatedAt: snapshotFixedNow.Add(
				-time.Duration(i+1) * time.Hour,
			),
		}
	}
	lm := snapListModel(issues)
	lm.cursor = 25 // mid-list so the scroll window has both start and end visible
	got := lm.View(120, 30, snapViewChrome())
	assertGolden(t, "list-scroll-indicator", got)
}

// ptrFormat is a tiny helper for fixture row titles that need the
// issue number embedded — keeps the fixture builder readable.
func ptrFormat(n int64) string {
	return fmt.Sprintf("%d", n)
}

func TestSnapshot_List_WithFilterChips(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel([]Issue{{
		UID:       "01TEST-aaa1",
		ShortID:   "aaa1",
		Title:     "narrowed by chips",
		Status:    "open",
		Owner:     ptrString("alice"),
		UpdatedAt: snapshotFixedNow.Add(-30 * time.Minute),
	}})
	lm.filter = ListFilter{Status: "open", Owner: "alice"}
	got := lm.View(120, 30, snapViewChrome())
	assertGolden(t, "list-with-filter-chips", got)
}

func TestSnapshot_List_TreeCollapsed(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapTreeFixture())
	got := lm.View(120, 22, snapViewChrome())
	assertGolden(t, "list-tree-collapsed", got)
}

func TestSnapshot_List_TreeExpanded(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapTreeFixture())
	lm.expanded = expansionSet{{projectID: 7, shortID: "p010"}: true}
	got := lm.View(120, 22, snapViewChrome())
	assertGolden(t, "list-tree-expanded", got)
}

func TestSnapshot_List_TreeAutoExpandedMatch(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapTreeFixture())
	lm.filter = ListFilter{Search: "jump target"}
	got := lm.View(120, 22, snapViewChrome())
	assertGolden(t, "list-tree-auto-expanded-match", got)
}

func TestSnapshot_List_TreeContextRow(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapTreeFixture())
	lm.filter = ListFilter{Search: "hint bars"}
	got := lm.View(120, 22, snapViewChrome())
	assertGolden(t, "list-tree-context-row", got)
}

func TestSnapshot_List_TreeNoColor(t *testing.T) {
	defer snapshotInit(t)()
	lm := snapListModel(snapTreeFixture())
	lm.expanded = expansionSet{{projectID: 7, shortID: "p010"}: true}
	lm.filter = ListFilter{Search: "hint bars"}
	got := lm.View(120, 22, snapViewChrome())
	if !strings.Contains(got, "-") || !strings.Contains(got, "~") {
		t.Fatalf("no-color tree snapshot missing fallback disclosure or context marker:\n%s", got)
	}
	assertGolden(t, "list-tree-no-color", got)
}

// TestSnapshot_Detail_WithLabelPrompt covers the M3b panel-local
// prompt rendered at the bottom of the detail pane.
func TestSnapshot_Detail_WithLabelPrompt(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	chrome := viewChrome{
		input: newPanelPrompt(inputLabelPrompt, formTarget{
			projectID: dm.scopePID, issueShortID: dm.issue.ShortID,
		}),
	}
	got := dm.View(120, 30, chrome)
	assertGolden(t, "detail-with-label-prompt", got)
}

func TestSnapshot_Detail_WithChildren(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailHierarchyFixture()
	got := dm.View(120, 32, viewChrome{})
	assertGolden(t, "detail-with-children", got)
}

func TestSnapshot_Detail_ChildrenFocus(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailHierarchyFixture()
	dm.detailFocus = focusChildren
	dm.childCursor = 1
	got := dm.View(120, 32, viewChrome{})
	assertGolden(t, "detail-children-focus", got)
}

// TestSnapshot_Detail_LongCommentsList locks the per-tab scroll
// indicator: 30 comments on a 30-row terminal forces the visible
// window to slice into the entries, and the footer shows
// `[start-end of N comments]`.
func TestSnapshot_Detail_LongCommentsList(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	when := time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	cs := make([]CommentEntry, 30)
	for i := range cs {
		cs[i] = CommentEntry{
			ID:        int64(i + 1),
			Author:    "actor-" + ptrFormat(int64(i+1)),
			Body:      "comment body " + ptrFormat(int64(i+1)),
			CreatedAt: when.Add(time.Duration(i) * time.Minute),
		}
	}
	dm.comments = cs
	dm.activeTab = tabComments
	dm.tabCursor = 14 // mid-list
	got := dm.View(120, 30, viewChrome{})
	assertGolden(t, "detail-long-comments-list", got)
}

// TestSnapshot_Detail_CommentsTab locks the comments tab render. Tab
// strip shows Comments highlighted; the entry list contains the two
// fixture comments with author, timestamp, and indented body lines.
func TestSnapshot_Detail_CommentsTab(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.activeTab = tabComments
	got := dm.View(120, 30, viewChrome{})
	assertGolden(t, "detail-comments-tab", got)
}

// TestSnapshot_Detail_EventsTab same fixture, events tab active.
func TestSnapshot_Detail_EventsTab(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.activeTab = tabEvents
	got := dm.View(120, 30, viewChrome{})
	assertGolden(t, "detail-events-tab", got)
}

// TestSnapshot_Detail_LinksTab same fixture, links tab active.
func TestSnapshot_Detail_LinksTab(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.activeTab = tabLinks
	got := dm.View(120, 30, viewChrome{})
	assertGolden(t, "detail-links-tab", got)
}

// TestSnapshot_Detail_WithLabels exercises the assignment row's chip
// strip on a wide terminal: owner left, three sorted chips right.
func TestSnapshot_Detail_WithLabels(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"prio-1", "bug", "needs-design"}
	got := dm.View(120, 30, viewChrome{})
	assertGolden(t, "detail-with-labels", got)
}

func TestSnapshot_Detail_DocumentPage80x50(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailHierarchyFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"prio-1", "bug", "needs-design"}
	dm.issue.CreatedAt = time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
	dm.issue.UpdatedAt = snapshotFixedNow.Add(-3 * time.Hour)
	got := dm.View(80, 50, viewChrome{
		scope:   scope{projectID: 7, projectName: "kata"},
		version: "dev",
	})
	assertGolden(t, "detail-document-80x50", got)
}

// TestSnapshot_Detail_DocumentWide160x32 locks down the redesigned
// stacked detail at the screenshot-scale terminal that previous
// iterations failed in. The handoff cited a wide live render with
// no project bar, no gutter, and `��` artifacts on the metadata
// rows; this golden is the canonical "what the user should see"
// reference for that scale. The fixture flows through applyFetched
// so the auto-select-first-non-empty-activity-tab path runs (the
// real binary's first-open behavior) and Events lights up instead
// of an empty Comments tab.
func TestSnapshot_Detail_DocumentWide160x32(t *testing.T) {
	defer snapshotInit(t)()
	iss := Issue{
		ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1",
		Title: "fix the tui to be less shitty", Status: "open",
		Author:    "anonymous",
		Body:      "enter some stuff here",
		CreatedAt: time.Date(2026, 5, 2, 19, 15, 0, 0, time.UTC),
		UpdatedAt: snapshotFixedNow.Add(-21 * time.Hour),
	}
	dm := detailModel{issue: &iss, scopePID: 7}
	dm = seedActivity(dm, nil, []EventLogEntry{
		{
			ID: 1, Type: "issue.created", Actor: "anonymous",
			CreatedAt: time.Date(2026, 5, 2, 19, 16, 0, 0, time.UTC),
		},
	}, nil)
	got := dm.View(160, 32, viewChrome{
		scope:   scope{projectID: 7, projectName: "kata"},
		version: "dev",
	})
	assertGolden(t, "detail-document-wide-160x32", got)
}

func TestSnapshot_Detail_DocumentNarrow(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{"bug", "prio-1"}
	dm.parent = &IssueRef{UID: "01TEST-c012", ShortID: "c012", Title: "workspace polish", Status: "open"}
	got := dm.View(72, 40, viewChrome{})
	assertGolden(t, "detail-document-narrow", got)
}

func TestSnapshot_Detail_DocumentEmpty(t *testing.T) {
	defer snapshotInit(t)()
	iss := Issue{ProjectID: 7, UID: "01TEST-99zz", ShortID: "99zz", Title: "empty issue", Status: "open"}
	dm := detailModel{issue: &iss}
	got := dm.View(80, 32, viewChrome{})
	assertGolden(t, "detail-document-empty", got)
}

func TestSnapshot_Detail_DocumentMarkdown(t *testing.T) {
	defer snapshotInit(t)()
	iss := Issue{
		ProjectID: 7,
		UID:       "01TEST-mm55",
		ShortID:   "mm55",
		Title:     "markdown body",
		Status:    "open",
		Body: strings.Join([]string{
			"## Steps",
			"",
			"- Click `Login` twice.",
			"",
			"```go",
			`fmt.Println("ok")`,
			"```",
		}, "\n"),
	}
	dm := detailModel{issue: &iss}
	got := dm.View(80, 40, viewChrome{})
	assertGolden(t, "detail-document-markdown", got)
}

// TestSnapshot_Detail_LabelsNarrow_OverflowAndDegrade verifies the
// chip strip degrades gracefully on narrow terminals: at 60 cells
// the +N overflow appears; at 20 cells the strip collapses to the
// `[N labels]` ultra-narrow fallback. Heights are deliberately tall
// enough that the comprehensive detail footer (which reflows to
// one item per row at narrow widths) does not crowd the labels
// row out of the document body — the test is about width-driven
// chip behaviour, not height-driven layout collapse.
func TestSnapshot_Detail_LabelsNarrow_OverflowAndDegrade(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	dm.issue.Labels = []string{
		"alpha-pretty-long", "beta-pretty-long", "gamma-pretty-long",
		"delta-pretty-long", "epsilon-pretty-long",
	}
	overflow := dm.View(60, 40, viewChrome{})
	if !strings.Contains(overflow, "+") {
		t.Fatalf("expected +N overflow at width 60, got:\n%s", overflow)
	}
	degrade := dm.View(20, 60, viewChrome{})
	if !strings.Contains(degrade, "labels]") {
		t.Fatalf("expected [N labels] degrade at width 20, got:\n%s", degrade)
	}
}

// TestSnapshot_Help_Narrow renders help at width 60 (helpColumnCount=1).
// Sections stack vertically.
func TestSnapshot_Help_Narrow(t *testing.T) {
	defer snapshotInit(t)()
	got := renderHelp(newKeymap(), 60, ListFilter{})
	assertGolden(t, "help-narrow", got)
}

// TestSnapshot_Help_Wide renders help at width 120 (helpColumnCount=3).
// Sections lay out side-by-side.
func TestSnapshot_Help_Wide(t *testing.T) {
	defer snapshotInit(t)()
	got := renderHelp(newKeymap(), 120, ListFilter{})
	assertGolden(t, "help-wide", got)
}

// TestSnapshot_Empty renders the onboarding empty state at 80x24.
func TestSnapshot_Empty(t *testing.T) {
	defer snapshotInit(t)()
	got := renderEmpty(80, 24)
	assertGolden(t, "empty-state", got)
}

// TestSnapshot_NarrowTerminalHint locks the M5 degraded hint at the
// canonical narrow fixture (60x24 — below the 80-cell width
// threshold). The bordered panel sits centered; q/ctrl+c routing is
// unaffected (covered by narrow_terminal_test.go).
func TestSnapshot_NarrowTerminalHint(t *testing.T) {
	defer snapshotInit(t)()
	got := renderTooNarrow(60, 24)
	assertGolden(t, "narrow-terminal-hint", got)
}

// TestSnapshot_LabelPrompt_MenuOpen renders the autocomplete menu
// for a `+` label prompt with 5 suggestions and the highlight on
// the first row. Pinned to 120x30 like the other detail snapshots.
func TestSnapshot_LabelPrompt_MenuOpen(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1,
		labels: []LabelCount{
			{Label: "alpha", Count: 5},
			{Label: "beta", Count: 4},
			{Label: "gamma", Count: 3},
			{Label: "delta", Count: 2},
			{Label: "epsilon", Count: 1},
		},
	}
	got := m.viewContent()
	assertGolden(t, "label-prompt-menu-open", got)
}

// TestSnapshot_LabelPrompt_Loading renders the loading-placeholder
// menu state — the cache is fetching but has no entries yet.
func TestSnapshot_LabelPrompt_Loading(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1, fetching: true,
	}
	got := m.viewContent()
	assertGolden(t, "label-prompt-loading", got)
}

// TestSnapshot_LabelPrompt_Error renders the error-placeholder menu
// state — the cache has an err and no labels.
func TestSnapshot_LabelPrompt_Error(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1, err: errStub("daemon 500"),
	}
	got := m.viewContent()
	assertGolden(t, "label-prompt-error", got)
}

// TestSnapshot_LabelPrompt_Empty renders the empty-placeholder menu
// state — the cache fetched, has no entries, no error.
func TestSnapshot_LabelPrompt_Empty(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1, fetching: false,
	}
	got := m.viewContent()
	assertGolden(t, "label-prompt-empty", got)
}

// TestSnapshot_LabelPrompt_Scroll renders the menu with 12
// suggestions and the highlight at index 9 — the visible window
// scrolls past the first entries.
func TestSnapshot_LabelPrompt_Scroll(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	suggestions := make([]LabelCount, 12)
	for i := range suggestions {
		suggestions[i] = LabelCount{
			Label: "lbl-" + ptrFormat(int64(i+1)),
			Count: int64(20 - i),
		}
	}
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1, labels: suggestions,
	}
	m.input.suggestHighlight = 9
	got := m.viewContent()
	assertGolden(t, "label-prompt-scroll", got)
}

// snapSplitModel builds the M6 split-mode Model used by the split
// snapshot tests: list fixture + open detail (snap fixture) + size
// pinned to the test's terminal dimensions, layout already decided
// by pickLayout. The caller picks the focus.
func snapSplitModel(width, height int, focus focusPane) Model {
	dm := snapDetailFixture()
	m := initialModel(Options{})
	m.scope = scope{projectID: 7, projectName: "kata"}
	m.list.loading = false
	m.list.issues = snapListFixture()
	m.list.cursor = 1
	m.list.selectedUID = "01TEST-bbb2"
	m.detail = dm
	m.width, m.height = width, height
	m.layout = pickLayout(width, height)
	m.focus = focus
	m.sseStatus = sseConnected
	return m
}

// TestSnapshot_Split_Wide locks the split rendering at 160x40 with
// focus on the list pane.
func TestSnapshot_Split_Wide(t *testing.T) {
	defer snapshotInit(t)()
	m := snapSplitModel(160, 40, focusList)
	got := m.viewContent()
	assertGolden(t, "list-detail-split-wide", got)
}

// TestSnapshot_Split_FocusDetail locks the split rendering at 160x40
// with focus on the detail pane (the borders should swap colors but
// snapshots run under colorNone so we mainly assert the body shape).
func TestSnapshot_Split_FocusDetail(t *testing.T) {
	defer snapshotInit(t)()
	m := snapSplitModel(160, 40, focusDetail)
	got := m.viewContent()
	assertGolden(t, "list-detail-split-focus-detail", got)
}

// TestSnapshot_Split_AtBreakpoint locks the split rendering at the
// minimum split-mode terminal size (140x36). This pin catches any
// regression where the breakpoint thresholds shift unexpectedly.
func TestSnapshot_Split_AtBreakpoint(t *testing.T) {
	defer snapshotInit(t)()
	m := snapSplitModel(140, 36, focusList)
	got := m.viewContent()
	assertGolden(t, "list-detail-split-resize-collapse", got)
}

// snapLabelPromptModel builds the Model used by the label-prompt
// snapshot tests: detail view with the snap fixture issue + a `+`
// prompt open against project 7 / issue 42. width/height are pinned
// at 120x30.
func snapLabelPromptModel() Model {
	dm := snapDetailFixture()
	m := Model{
		view:          viewDetail,
		scope:         scope{projectID: 7, projectName: "kata"},
		width:         120,
		height:        30,
		sseStatus:     sseConnected,
		projectLabels: newLabelCache(),
		detail:        dm,
	}
	m.input = newPanelPrompt(inputLabelPrompt, formTarget{
		projectID: 7, issueShortID: dm.issue.ShortID, detailGen: dm.gen,
	})
	return m
}
