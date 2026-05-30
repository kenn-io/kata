package tui

import (
	"io"
	"strings"
	"testing"
	"unicode"

	"github.com/mattn/go-runewidth"
)

// TestRenderLabelChips_AlphabeticalSort verifies the input slice is
// rendered alphabetically regardless of caller order. Sort is in-render
// so callers don't have to pre-sort the daemon's response.
func TestRenderLabelChips_AlphabeticalSort(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"prio-1", "bug", "needs-design"}, 80)
	bug := strings.Index(got, "[bug]")
	needs := strings.Index(got, "[needs-design]")
	prio := strings.Index(got, "[prio-1]")
	if bug < 0 || needs < 0 || prio < 0 {
		t.Fatalf("missing chip(s) in %q", got)
	}
	if bug >= needs || needs >= prio {
		t.Fatalf("chips out of order in %q (bug=%d needs=%d prio=%d)",
			got, bug, needs, prio)
	}
}

// TestRenderLabelChips_PacksUntilOverflow narrows the available width
// so not every chip fits; the renderer must drop the tail and append
// `+N` indicating the count of dropped labels.
func TestRenderLabelChips_PacksUntilOverflow(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"a", "b", "c", "d", "e"}, 12)
	if !strings.Contains(got, "+") {
		t.Fatalf("expected +N overflow marker in %q", got)
	}
	// Width budget caps total visible cells at 12.
	if w := runewidth.StringWidth(got); w > 12 {
		t.Fatalf("rendered width %d exceeds budget 12: %q", w, got)
	}
}

// TestRenderLabelChips_PlusNOverflowFormat verifies the +N token is
// formed correctly (literal +, then a base-10 integer >= 1) when the
// chip pack drops chips.
func TestRenderLabelChips_PlusNOverflowFormat(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"alpha", "beta", "gamma", "delta"}, 14)
	idx := strings.Index(got, "+")
	if idx < 0 {
		t.Fatalf("no +N token in %q", got)
	}
	rest := strings.TrimSpace(got[idx+1:])
	// Must start with a digit and parse as a positive integer.
	if rest == "" || rest[0] < '0' || rest[0] > '9' {
		t.Fatalf("+N suffix is not a number in %q", got)
	}
}

// TestRenderLabelChips_UltraNarrowFallback verifies the "[N labels]"
// degraded form when even one chip won't fit. The fallback keeps the
// header informative on tiny terminals.
func TestRenderLabelChips_UltraNarrowFallback(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"bug", "prio-1"}, 5)
	if !strings.Contains(got, "[2 labels]") {
		t.Fatalf("expected ultra-narrow fallback [2 labels] in %q", got)
	}
}

// TestRenderLabelChips_EmptyLabels verifies the empty-labels placeholder
// renders so the header layout doesn't shift when labels are absent.
func TestRenderLabelChips_EmptyLabels(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips(nil, 80)
	if !strings.Contains(got, "(no labels)") {
		t.Fatalf("expected (no labels) placeholder in %q", got)
	}
}

// TestRenderLabelChips_WidthMeasureUsesRunewidth pins the width-math
// invariant: a wide-glyph label (`カタ` is 4 cells, not 6 bytes) plus
// an embedded ANSI escape must be measured correctly. Width is
// computed AFTER sanitize so the measurement matches the rendered cell.
//
// Sorted clean labels: "bug" (3 cells), "カタ" (4 cells). At width 11:
//   - "[bug]" = 5 cells; reserve 4-cell overflow tail since one label
//     remains. 0+5+4 = 9 <= 11, so "[bug]" fits (used=5).
//   - "[カタ]" = 6 cells + 1-cell separator = 7. 5+7+0 = 12 > 11, so
//     "[カタ]" is dropped. Output: "[bug] +1".
//
// If the renderer measured the raw `\x1b[31mカタ` (10+ bytes) instead
// of the sanitized "カタ" (4 cells), or measured byte length instead
// of cell width, the math would be wrong and the test would fail.
func TestRenderLabelChips_WidthMeasureUsesRunewidth(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"\x1b[31mカタ", "bug"}, 11)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ESC survived width-measure path: %q", got)
	}
	assertContainsAll(t, got, "[bug]", "+1")
	if w := runewidth.StringWidth(got); w > 11 {
		t.Fatalf("rendered width %d exceeds budget 11: %q", w, got)
	}
	// Direct width check on the sanitized label proves we really are
	// measuring cell width, not bytes.
	if cells := runewidth.StringWidth("カタ"); cells != 4 {
		t.Fatalf("runewidth.StringWidth(\"カタ\") = %d, want 4", cells)
	}
}

// TestRenderLabelChips_RenderedTextSanitized proves the chip TEXT is
// sanitized — not just the width measurement. Hostile labels with ANSI
// escapes and a U+202E RIGHT-TO-LEFT OVERRIDE must not survive into
// the rendered output.
func TestRenderLabelChips_RenderedTextSanitized(t *testing.T) {
	useNoColor(t)
	rlo := rune(0x202E)
	hostile := "ok" + string(rlo) + "pad"
	got := renderLabelChips([]string{"bug\x1b[2J", hostile}, 80)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("ESC reached rendered chips: %q", got)
	}
	if strings.ContainsRune(got, rlo) {
		t.Fatalf("U+202E survived: %q", got)
	}
	for _, r := range got {
		if unicode.Is(unicode.Cf, r) {
			t.Fatalf("Cf rune %U survived in rendered chips: %q", r, got)
		}
	}
}

// TestTitleBarLeft_SanitizeEmptyFallsBackToPlaceholder pins roborev
// job 128: when sc.projectName sanitizes down to "" (control runes
// only, zero-width joiners only, etc.), the renderer must render
// `Project: —` rather than the empty `Project: ` form, preserving
// the "left side never blank" invariant.
func TestTitleBarLeft_SanitizeEmptyFallsBackToPlaceholder(t *testing.T) {
	useNoColor(t)
	// String of pure control runes — sanitizeForDisplay strips them.
	got := titleBarLeft(scope{projectName: "\x01\x02\x07"})
	if got != "Project: —" {
		t.Fatalf("titleBarLeft control-only name = %q, want %q", got, "Project: —")
	}
	got2 := titleBarLeft(scope{projectName: ""})
	if got2 != "Project: —" {
		t.Fatalf("titleBarLeft empty name = %q, want %q", got2, "Project: —")
	}
	got3 := titleBarLeft(scope{projectName: "kata"})
	if got3 != "Project: kata" {
		t.Fatalf("titleBarLeft happy path = %q, want %q", got3, "Project: kata")
	}
	got4 := titleBarLeft(scope{allProjects: true})
	if got4 != "Project: all" {
		t.Fatalf("titleBarLeft all-projects = %q, want %q", got4, "Project: all")
	}
}

func TestTitleBarShowsDaemonName(t *testing.T) {
	useNoColor(t)
	got := renderTitleBar(100, scope{projectName: "kata"}, "dev", "shared")

	assertContains(t, stripANSI(got), "Daemon: shared", "title bar missing configured daemon name")
}

func TestTitleBarShowsDaemonHostFallback(t *testing.T) {
	useNoColor(t)
	got := renderTitleBar(120, scope{projectName: "kata"}, "dev", "daemon.internal:7777")

	assertContains(t, stripANSI(got), "Daemon: daemon.internal:7777", "title bar missing daemon host fallback")
}

func TestTitleBarShowsLocalDaemonFallback(t *testing.T) {
	useNoColor(t)
	m := initialModel(Options{})
	m.scope = scope{projectName: "kata"}
	m.activeDaemon = daemonTarget{Local: true}
	m.list.loading = false

	got := m.list.View(120, 12, m.chrome())

	assertContains(t, stripANSI(got), "Daemon: local", "title bar missing local daemon fallback")
}

func TestTitleBarSanitizesDaemonLabel(t *testing.T) {
	useNoColor(t)
	got := renderTitleBar(100, scope{projectName: "kata"}, "dev", "shared\nbad")

	assertContains(t, stripANSI(got), `Daemon: shared\nbad`, "title bar did not sanitize daemon label")
}

func TestDetailTitleBarShowsDaemon(t *testing.T) {
	useNoColor(t)
	iss := testIssue("abc1")
	dm := detailModel{issue: &iss}

	got := dm.View(100, 18, viewChrome{
		scope:   scope{projectName: "kata"},
		version: "dev",
		daemon:  "shared",
	})

	assertContains(t, stripANSI(got), "Daemon: shared", "detail title bar missing daemon label")
}

func TestSplitTitleBarShowsDaemon(t *testing.T) {
	useNoColor(t)
	m := resizeModel(newTestModel(), 130, 30)
	m.layout = layoutSplit
	m.activeDaemon = daemonTarget{Name: "shared"}
	m.list.issues = []Issue{testIssue("abc1")}
	iss := testIssue("abc1")
	m.detail.issue = &iss

	got := renderSplit(m)

	assertContains(t, stripANSI(got), "Daemon: shared", "split title bar missing daemon label")
}

// TestRenderLabelChips_LargeOverflowReservesActualWidth pins the
// fix for roborev job 235: with >=100 labels dropped, the `+N` token
// is `+100` (5 cells) — wider than the legacy hard-coded 4-cell
// reserve. The render must compute reserve from len(clean) so the
// final width never exceeds the budget.
//
// Budget is tuned to the boundary where the old 4-cell reserve and
// the new dynamic reserve diverge: with 150 "[bug]" chips (5 cells
// each, 1-cell separator) at budget=15, the old reserve packed 2
// chips (used=11) leaving "+148" (4 cells) for total=17 > 15 — an
// actual budget overrun. The new reserve packs 1 chip (used=5)
// leaving " +149" for total=10. Job 248 callout: an earlier draft
// of this test used budget=30 where both reserves happened to pack
// the same number of chips, so it would not catch the regression.
func TestRenderLabelChips_LargeOverflowReservesActualWidth(t *testing.T) {
	useNoColor(t)
	labels := make([]string, 150)
	for i := range labels {
		labels[i] = "bug"
	}
	const budget = 15
	got := renderLabelChips(labels, budget)
	if w := runewidth.StringWidth(got); w > budget {
		t.Fatalf("rendered width %d exceeds budget %d: %q", w, budget, got)
	}
	if !strings.Contains(got, "+") {
		t.Fatalf("expected +N overflow marker in %q", got)
	}
}

// TestRenderLabelChips_NewlineInLabelDoesNotBreakRow pins the
// defense-in-depth invariant that a label containing a literal newline
// cannot split a chip across two terminal rows. The chip strip is a
// single-row context, so the renderer must source chip text through
// textsafe.Line (which replaces \n with literal "\n") rather than
// textsafe.Block (which preserves \n for multi-line bodies). The
// schema bars newlines in labels (SQLite CHECK at 0001_init.sql:103)
// but the TUI is the wrong layer to depend on that; this test guards
// the renderer-level invariant directly.
func TestRenderLabelChips_NewlineInLabelDoesNotBreakRow(t *testing.T) {
	useNoColor(t)
	got := renderLabelChips([]string{"bug\nfoo"}, 80)
	if strings.ContainsRune(got, '\n') {
		t.Fatalf("literal newline survived in chip strip: %q", got)
	}
	if !strings.Contains(got, `\n`) {
		t.Fatalf("expected literal escape sequence \\n in rendered chip: %q", got)
	}
}

func TestDisclosureGlyph(t *testing.T) {
	applyColorMode(colorAuto, io.Discard)
	if got := disclosureGlyph(false, false); got != " " {
		t.Fatalf("leaf glyph = %q, want blank", got)
	}
	if got := disclosureGlyph(true, false); got != "▸" {
		t.Fatalf("collapsed glyph = %q, want ▸", got)
	}
	if got := disclosureGlyph(true, true); got != "▾" {
		t.Fatalf("expanded glyph = %q, want ▾", got)
	}

	useNoColor(t)
	if got := disclosureGlyph(true, false); got != "+" {
		t.Fatalf("no-color collapsed glyph = %q, want +", got)
	}
	if got := disclosureGlyph(true, true); got != "-" {
		t.Fatalf("no-color expanded glyph = %q, want -", got)
	}
}

// TestNavTreeSlot_ChildGuides verifies that child rows render explicit
// connectors inside the compact nav gutter. The old folded-gutter render
// gave leaf children only blank space, making a single child hard to
// distinguish from an unrelated sibling row below.
func TestNavTreeSlot_ChildGuides(t *testing.T) {
	applyColorMode(colorAuto, io.Discard)
	parent := queueRow{depth: 0, hasChildren: true, expanded: true}
	got := stripANSI(navTreeSlot(parent))
	if got != "▾" {
		t.Fatalf("expanded parent navTreeSlot = %q, want ▾", got)
	}
	intermediate := queueRow{depth: 1, lastChild: false}
	got = stripANSI(navTreeSlot(intermediate))
	if got != "├─" {
		t.Fatalf("intermediate child navTreeSlot = %q, want ├─", got)
	}
	last := queueRow{depth: 1, lastChild: true}
	got = stripANSI(navTreeSlot(last))
	if got != "└─" {
		t.Fatalf("last child navTreeSlot = %q, want └─", got)
	}

	deep := queueRow{depth: 2, lastChild: true}
	got = stripANSI(navTreeSlot(deep))
	if got != "…└" {
		t.Fatalf("depth-2 last child navTreeSlot = %q, want …└", got)
	}

	childParent := queueRow{depth: 1, hasChildren: true, expanded: true, lastChild: false}
	got = stripANSI(navTreeSlot(childParent))
	if got != "├▾" {
		t.Fatalf("expanded child parent navTreeSlot = %q, want ├▾", got)
	}
	collapsedLastChildParent := queueRow{depth: 1, hasChildren: true, expanded: false, lastChild: true}
	got = stripANSI(navTreeSlot(collapsedLastChildParent))
	if got != "└▸" {
		t.Fatalf("collapsed last-child parent navTreeSlot = %q, want └▸", got)
	}
	deepParent := queueRow{depth: 2, hasChildren: true, expanded: true, lastChild: true}
	got = stripANSI(navTreeSlot(deepParent))
	if got != "…▾" {
		t.Fatalf("expanded depth-2 child parent navTreeSlot = %q, want …▾", got)
	}

	useNoColor(t)
	got = navTreeSlot(queueRow{depth: 1, lastChild: false})
	if got != "+-" {
		t.Fatalf("no-color intermediate child = %q, want +-", got)
	}
	got = navTreeSlot(queueRow{depth: 1, lastChild: true})
	if got != `\-` {
		t.Fatalf(`no-color last child = %q, want \-`, got)
	}
	got = navTreeSlot(queueRow{depth: 2, lastChild: true})
	if got != `.\` {
		t.Fatalf(`no-color depth-2 last child = %q, want .\`, got)
	}
	if strings.ContainsRune(got, '…') {
		t.Fatalf("no-color depth-2 contains U+2026: %q", got)
	}
}

// TestGroupBanding_ParentChildShareBand verifies that an expanded
// parent and its expanded children render with the same banding class.
func TestGroupBanding_ParentChildShareBand(t *testing.T) {
	visible := []queueRow{
		{depth: 0}, // root A
		{depth: 1}, // child of A
		{depth: 1}, // child of A
		{depth: 0}, // root B
		{depth: 0}, // root C
		{depth: 1}, // child of C
	}
	bands := groupBanding(visible)
	if len(bands) != len(visible) {
		t.Fatalf("len = %d, want %d", len(bands), len(visible))
	}
	if bands[0] != bands[1] || bands[1] != bands[2] {
		t.Fatalf("group A rows differ: %v", bands[:3])
	}
	if bands[0] == bands[3] {
		t.Fatalf("root A and root B should flip bands; got %v / %v", bands[0], bands[3])
	}
	if bands[3] == bands[4] {
		t.Fatalf("consecutive roots B/C should flip bands; got %v / %v", bands[3], bands[4])
	}
	if bands[4] != bands[5] {
		t.Fatalf("root C and its child should share band; got %v / %v", bands[4], bands[5])
	}

	_, _, windowStart := windowQueueRows(visible, 3, 3)
	windowBands := bands[windowStart : windowStart+3]
	if windowStart != 2 {
		t.Fatalf("windowStart = %d, want 2", windowStart)
	}
	if windowBands[0] != bands[0] {
		t.Fatalf("child row band changed at viewport top: window %v full %v", windowBands, bands)
	}
	if windowBands[1] != bands[3] {
		t.Fatalf("root B band should remain absolute after slicing: window %v full %v", windowBands, bands)
	}
}

func TestRenderListBody_UsesQueueRowsWithDisclosureAndChildCounts(t *testing.T) {
	useNoColor(t)
	parentSID := "p001"
	lm := listModel{issues: []Issue{
		{
			ProjectID: 7, UID: "01TEST-p001", ShortID: parentSID, Title: "parent", Status: "open",
			ChildCounts: &ChildCounts{Open: 1, Total: 2},
		},
		{ProjectID: 7, UID: "01TEST-c002", ShortID: "c002", ParentShortID: &parentSID, Title: "child", Status: "open"},
	}}

	collapsed := renderBodyNoColor(lm, 100, 6, viewChrome{})
	assertContainsAll(t, collapsed, "+", "1/2")
	assertStringsLack(t, collapsed, "child")

	lm.expanded = expansionSet{{projectID: 7, shortID: parentSID}: true}
	expanded := renderBodyNoColor(lm, 100, 6, viewChrome{})
	assertContainsAll(t, expanded, "-", "child")
}

func TestRenderListBody_ContextRowHasVisibleMarkerInNoColor(t *testing.T) {
	useNoColor(t)
	parentSID := "p001"
	lm := listModel{
		issues: []Issue{
			{ProjectID: 7, UID: "01TEST-p001", ShortID: parentSID, Title: "parent", Status: "open"},
			{ProjectID: 7, UID: "01TEST-c002", ShortID: "c002", ParentShortID: &parentSID, Title: "child login", Status: "open"},
		},
		filter: ListFilter{Search: "login"},
	}

	got := renderBodyNoColor(lm, 100, 6, viewChrome{})
	assertContainsAll(t, got, "~", "parent", "child login")
}

// TestRenderListBody_AllProjectsPrefixesTitle pins the navigation contract
// for the R toggle: in all-projects scope each row's title is prefixed with
// the owning project's display name from chrome.projectsByID, so the user
// can tell which project a row belongs to without expanding detail.
func TestRenderListBody_AllProjectsPrefixesTitle(t *testing.T) {
	useNoColor(t)
	lm := listModel{issues: []Issue{
		{ProjectID: 7, UID: "01TEST-7aaa", ShortID: "7aaa", Title: "alpha bug", Status: "open"},
		{ProjectID: 9, UID: "01TEST-9aaa", ShortID: "9aaa", Title: "beta bug", Status: "open"},
	}}
	chrome := viewChrome{
		scope:        scope{allProjects: true},
		projectsByID: map[int64]string{7: "alpha", 9: "beta"},
	}
	got := renderBodyNoColor(lm, 120, 6, chrome)
	assertContainsAll(t, got, "[alpha] alpha bug", "[beta] beta bug")
}

// TestRenderListBody_AllProjectsFallsBackToPID pins the degraded path: a
// row whose project is missing from the cache (e.g. a freshly-created
// project before the next /projects refresh) renders as "[#PID]" rather
// than appearing nameless.
func TestRenderListBody_AllProjectsFallsBackToPID(t *testing.T) {
	useNoColor(t)
	lm := listModel{issues: []Issue{
		{ProjectID: 42, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "ghost project", Status: "open"},
	}}
	chrome := viewChrome{
		scope:        scope{allProjects: true},
		projectsByID: map[int64]string{},
	}
	got := renderBodyNoColor(lm, 120, 6, chrome)
	if !strings.Contains(got, "[#42] ghost project") {
		t.Fatalf("missing-project render must fall back to [#PID]:\n%s", got)
	}
}

// TestRenderListBody_SingleProjectOmitsPrefix pins the inverse: in
// single-project scope the prefix is omitted (every row belongs to the
// same project, so repeating the name is noise).
func TestRenderListBody_SingleProjectOmitsPrefix(t *testing.T) {
	useNoColor(t)
	lm := listModel{issues: []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha bug", Status: "open"},
	}}
	chrome := viewChrome{
		scope:        scope{projectID: 7, projectName: "alpha"},
		projectsByID: map[int64]string{7: "alpha"},
	}
	got := renderBodyNoColor(lm, 120, 6, chrome)
	assertStringsLack(t, got, "[alpha]")
	if !strings.Contains(got, "alpha bug") {
		t.Fatalf("title missing in single-project render:\n%s", got)
	}
}

func TestRenderListBody_HeaderBackgroundReplacesSeparatorRule(t *testing.T) {
	applyColorMode(colorDark, io.Discard)
	if !styleHasBackground(tableHeaderStyle) {
		t.Fatal("tableHeaderStyle must carry a background in color modes")
	}

	lm := listModel{issues: []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "row", Status: "open"}}}
	got := stripANSI(lm.renderBody(80, 6, viewChrome{}))
	for _, line := range strings.Split(got, "\n") {
		if strings.Trim(line, "─") == "" && strings.Contains(line, "─") {
			t.Fatalf("renderBody still renders a separator rule:\n%s", got)
		}
	}
}

func TestListView_BodyBudgetCountsOnlyTableHeader(t *testing.T) {
	useNoColor(t)
	lm := listModel{issues: []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "one", Status: "open"},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "two", Status: "open"},
		{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "three", Status: "open"},
		{UID: "01TEST-ddd4", ShortID: "ddd4", Title: "four", Status: "open"},
	}}

	got := stripANSI(lm.View(100, 10, viewChrome{scope: scope{projectName: "kata"}}))
	assertContainsAll(t, got, "one", "two", "three", "four", "[1/4]")
	if strings.Contains(got, "[1-3 of 4]") {
		t.Fatalf("scroll indicator used stale header+separator budget:\n%s", got)
	}
}

func TestRenderListBody_EmptyStateDoesNotRenderSeparatorRule(t *testing.T) {
	applyColorMode(colorDark, io.Discard)
	lm := listModel{}
	got := stripANSI(lm.renderBody(80, 6, viewChrome{}))
	for _, line := range strings.Split(got, "\n") {
		if strings.Trim(line, "─") == "" && strings.Contains(line, "─") {
			t.Fatalf("empty renderBody still renders a separator rule:\n%s", got)
		}
	}
}

func TestRenderListInfoLine_TruncationNotice(t *testing.T) {
	useNoColor(t)
	lm := listModel{truncated: true, issues: []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1", Status: "open"}}}
	got := stripANSI(renderListInfoLine(100, viewChrome{}, lm, 10))
	want := "showing first 2000 issues; refine filters"
	if !strings.Contains(got, want) {
		t.Fatalf("truncation notice missing %q in %q", want, got)
	}
}

func TestListTableHeaders_UsesSingleNavGutter(t *testing.T) {
	wide := listTableHeaders(false)
	if len(wide) != 8 {
		t.Fatalf("wide headers len = %d, want 8 (%v)", len(wide), wide)
	}
	if wide[0] != "" || wide[1] != "#" || wide[2] != "prio" {
		t.Fatalf("wide headers should start with compact nav, #, then prio: %v", wide)
	}

	narrow := listTableHeaders(true)
	if len(narrow) != 7 {
		t.Fatalf("narrow headers len = %d, want 7 (%v)", len(narrow), narrow)
	}
	if narrow[0] != "" || narrow[1] != "#" || narrow[2] != "prio" {
		t.Fatalf("narrow headers should start with compact nav, #, then prio: %v", narrow)
	}
}

func TestBuildRows_FoldsSelectionContextAndDisclosureIntoNavCell(t *testing.T) {
	applyColorMode(colorNone, io.Discard)
	rows := buildRows([]queueRow{
		{
			issue:       Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Status: "open", Title: "context parent"},
			hasChildren: true,
			expanded:    true,
			context:     true,
		},
		{issue: Issue{UID: "01TEST-bbb2", ShortID: "bbb2", Status: "open", Title: "context leaf"}, context: true},
		{
			issue:       Issue{UID: "01TEST-ccc3", ShortID: "ccc3", Status: "open", Title: "context child parent"},
			depth:       1,
			hasChildren: true,
			context:     true,
			lastChild:   true,
		},
	}, 0, 20, false, viewChrome{})

	if len(rows[0]) != 8 {
		t.Fatalf("wide row column count = %d, want 8 (%v)", len(rows[0]), rows[0])
	}
	if rows[0][0] != "▶~-" {
		t.Fatalf("selected context parent nav = %q, want %q", rows[0][0], "▶~-")
	}
	if rows[1][0] != " ~ " {
		t.Fatalf("unselected context leaf nav = %q, want %q", rows[1][0], " ~ ")
	}
	if rows[2][0] != ` ~+` {
		t.Fatalf(`unselected context child parent nav = %q, want " ~+"`, rows[2][0])
	}
}
