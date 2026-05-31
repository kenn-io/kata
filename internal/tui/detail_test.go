package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-runewidth"
)

// newDetailHostModel builds a Model wired with a stub API client and a
// single-project scope so detail-level handler tests can drive
// Model.Update without re-typing the boilerplate.
func newDetailHostModel(opts Options, projectID int64) Model {
	m := initialModel(opts)
	m.api = &Client{}
	m.scope = scope{projectID: projectID}
	return m
}

// assertJumpDetailCmd evaluates cmd, asserts it returns a jumpDetailMsg
// targeting want, and fails otherwise.
func assertJumpDetailCmd(t *testing.T, cmd tea.Cmd, want string) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected jump cmd, got nil")
	}
	msg := cmd()
	jm, ok := msg.(jumpDetailMsg)
	if !ok {
		t.Fatalf("expected jumpDetailMsg, got %T", msg)
	}
	if jm.ref != want {
		t.Fatalf("jumpDetailMsg.ref = %q, want %q", jm.ref, want)
	}
}

// detailFixture seeds a detailModel with one issue, two comments, two
// events, and one link so every per-tab renderer has data to render.
// The body has many lines so j scrolling has somewhere to go.
func detailFixture() detailModel {
	iss := Issue{
		ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa",
		Title: "fix login bug on Safari", Status: "open", Author: "wesm",
	}
	body := strings.Repeat("line\n", 20) + "tail"
	iss.Body = body
	return detailModel{
		issue:    &iss,
		scopePID: 7,
		comments: []CommentEntry{
			{ID: 1, Author: "alice", Body: "first"},
			{ID: 2, Author: "bob", Body: "second"},
		},
		events: []EventLogEntry{
			{ID: 9, Type: "issue.created", Actor: "alice"},
			{ID: 10, Type: "issue.commented", Actor: "bob"},
		},
		links: []LinkEntry{
			{
				ID: 100, Type: "blocks",
				From:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
				To:     LinkPeer{UID: "01TEST-7bb", ShortID: "7bb"},
				Author: "wesm",
			},
		},
	}
}

// TestDetail_Render_Header_Title confirms the title appears in the view.
func TestDetail_Render_Header_Title(t *testing.T) {
	dm := detailFixture()
	out := dm.View(80, 24, viewChrome{})
	if !strings.Contains(out, "fix login bug on Safari") {
		t.Fatalf("title missing from view:\n%s", out)
	}
	if !strings.Contains(out, "#42aa") {
		t.Fatalf("issue short_id missing:\n%s", out)
	}
}

func TestDetail_RenderUIDDefaultHidden(t *testing.T) {
	dm := detailFixture()
	dm.issue.UID = "01JZ0000000000000000000001"
	out := stripANSI(dm.View(80, 24, viewChrome{}))
	if strings.Contains(out, "01JZ0000000000000000000001") ||
		strings.Contains(out, "~01JZ0000") {
		t.Fatalf("UID rendered with default format:\n%s", out)
	}
}

func TestDetail_RenderUIDShortFormat(t *testing.T) {
	dm := detailFixture()
	dm.issue.UID = "01JZ0000000000000000000001"
	dm.uidFormat = uidDisplayShort
	out := stripANSI(dm.View(80, 24, viewChrome{}))
	if !strings.Contains(out, "uid: ~01JZ0000") {
		t.Fatalf("short UID missing from detail metadata:\n%s", out)
	}
	if strings.Contains(out, "01JZ0000000000000000000001") {
		t.Fatalf("short UID render leaked full UID:\n%s", out)
	}
}

func TestDetail_RenderUIDFullFormat(t *testing.T) {
	dm := detailFixture()
	dm.issue.UID = "01JZ0000000000000000000001"
	dm.uidFormat = uidDisplayFull
	out := stripANSI(dm.View(80, 24, viewChrome{}))
	if !strings.Contains(out, "uid: 01JZ0000000000000000000001") {
		t.Fatalf("full UID missing from detail metadata:\n%s", out)
	}
}

func TestDetail_OpenCopiesConfiguredUIDFormat(t *testing.T) {
	m := initialModel(Options{DisplayUIDFormat: "short"})
	out, _ := m.handleOpenDetail(openDetailMsg{issue: Issue{
		ProjectID: 7,
		UID:       "01JZ0000000000000000000001",
		ShortID:   "42aa",
		Title:     "configured",
		Status:    "open",
	}})
	next := out.(Model)
	if next.detail.uidFormat != uidDisplayShort {
		t.Fatalf("detail uidFormat = %v, want short", next.detail.uidFormat)
	}
	rendered := stripANSI(next.detail.View(80, 24, viewChrome{}))
	if !strings.Contains(rendered, "uid: ~01JZ0000") {
		t.Fatalf("configured UID format not rendered:\n%s", rendered)
	}
}

func TestDetail_CompactSheetDoesNotRenderUID(t *testing.T) {
	iss := Issue{
		UID:     "01JZ0000000000000000000001",
		ShortID: "42aa",
		Title:   "compact",
		Status:  "open",
	}
	dm := detailModel{issue: &iss, uidFormat: uidDisplayFull}
	out := stripANSI(dm.renderTinyFallback(32))
	if strings.Contains(out, "01JZ0000000000000000000001") ||
		strings.Contains(out, "~01JZ0000") {
		t.Fatalf("compact sheet rendered UID:\n%s", out)
	}
}

func TestRenderHierarchySummary_FormatsParentAndChildren(t *testing.T) {
	parent := &IssueRef{ShortID: "12pp", Title: "workspace polish parent", Status: "open"}
	children := []Issue{
		{UID: "01TEST-43cc", ShortID: "43cc", Status: "open"},
		{UID: "01TEST-44dd", ShortID: "44dd", Status: "closed"},
	}
	got := renderHierarchySummary(80, parent, children)
	if !strings.Contains(got, "Parent: #12pp workspace polish parent") {
		t.Fatalf("parent summary missing:\n%s", got)
	}
	if !strings.Contains(got, "Children: 1 open / 2 total") {
		t.Fatalf("children summary missing:\n%s", got)
	}

	longParent := &IssueRef{ShortID: "99zz", Title: strings.Repeat("very long ", 20), Status: "open"}
	got = renderHierarchySummary(50, longParent, children)
	if runewidth.StringWidth(stripANSI(got)) > 50 {
		t.Fatalf("summary width overflowed: width=%d text=%q",
			runewidth.StringWidth(stripANSI(got)), got)
	}
}

func TestDetail_RenderHierarchySections(t *testing.T) {
	dm := detailFixture()
	dm.parent = &IssueRef{ShortID: "12pp", Title: "workspace polish parent", Status: "open"}
	dm.children = []Issue{
		{UID: "01TEST-43cc", ShortID: "43cc", Title: "detail hint bars incomplete", Status: "open", Owner: ptrString("alice")},
		{UID: "01TEST-44dd", ShortID: "44dd", Title: "new issue form parent field", Status: "closed"},
	}
	// In the unified-viewport layout the children section sits below the
	// body in document order. Assert against the assembled document
	// rather than the viewport so we don't have to size the terminal
	// large enough to fit everything (or scroll programmatically).
	docLines, _ := dm.detailDocumentLines(100, viewChrome{})
	out := stripANSI(strings.Join(docLines, "\n"))
	assertContainsAll(t, out,
		"parent: #12pp workspace polish parent",
		"children: 1 open / 2 total",
		"Children",
		"#43cc",
		"detail hint bars incomplete",
		"#44dd",
	)
}

// TestDetail_TabCycle_NextPrev: tab cycles forward, shift+tab cycles
// backward, both with wrap-around. Three forward presses returns to
// Comments; one backward from Comments lands on Links.
func TestDetail_TabCycle_NextPrev(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()
	tabKey := tea.KeyMsg{Type: tea.KeyTab}
	shiftTab := tea.KeyMsg{Type: tea.KeyShiftTab}

	dm, _ = dm.Update(tabKey, km, nil)
	if dm.activeTab != tabEvents {
		t.Fatalf("after tab: activeTab = %d, want tabEvents (%d)", dm.activeTab, tabEvents)
	}
	dm, _ = dm.Update(tabKey, km, nil)
	if dm.activeTab != tabLinks {
		t.Fatalf("after tab tab: activeTab = %d, want tabLinks (%d)", dm.activeTab, tabLinks)
	}
	dm, _ = dm.Update(tabKey, km, nil)
	if dm.activeTab != tabComments {
		t.Fatalf("after wrap: activeTab = %d, want tabComments (%d)", dm.activeTab, tabComments)
	}
	dm, _ = dm.Update(shiftTab, km, nil)
	if dm.activeTab != tabLinks {
		t.Fatalf("after shift+tab from comments: activeTab = %d, want tabLinks", dm.activeTab)
	}
}

// TestDetail_TabRender_ActiveContent: after a tab press the active-tab
// chip changes in the activity strip. Assert against the assembled
// document rather than the viewport so the activity strip is in scope
// regardless of body length.
func TestDetail_TabRender_ActiveContent(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()
	docOut := func(d detailModel) string {
		lines, _ := d.detailDocumentLines(80, viewChrome{})
		return stripANSI(strings.Join(lines, "\n"))
	}
	if !strings.Contains(docOut(dm), "Comments (2)") {
		t.Fatalf("comments header missing:\n%s", docOut(dm))
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if !strings.Contains(docOut(dm), "Events (2)") {
		t.Fatalf("events header missing after tab:\n%s", docOut(dm))
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if !strings.Contains(docOut(dm), "Links (1)") {
		t.Fatalf("links header missing after second tab:\n%s", docOut(dm))
	}
}

// TestDetail_Scroll_BoundsAtTop: with no comments, k at scroll==0 must
// clamp at zero. The fixture HAS comments so we use a body-only model.
func TestDetail_Scroll_BoundsAtTop(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Body: "x\ny"}}
	km := newKeymap()
	dm, _ = dm.Update(runeKey('k'), km, nil)
	if dm.scroll != 0 {
		t.Fatalf("scroll = %d, want 0 (clamped at top)", dm.scroll)
	}
}

// TestDetail_Scroll_DownIncreases: j increments dm.scroll WHEN the
// active tab has no rows. The fixture comments would steal j, so
// build a tab-empty model for the body-scroll path.
func TestDetail_Scroll_DownIncreases(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Body: "x\ny"}}
	km := newKeymap()
	dm, _ = dm.Update(runeKey('j'), km, nil)
	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.scroll != 2 {
		t.Fatalf("scroll = %d, want 2", dm.scroll)
	}
}

// TestDetail_Scroll_PageDownScrollsBodyEvenWithActivityRows: the
// dedicated body-scroll keys (pgup/pgdn) must scroll the body
// regardless of focus or whether the active tab has rows. The j/k
// path is reserved for the activity-tab cursor when the tab has data
// (so users can navigate comments/events/links by line); pgup/pgdn
// is the escape hatch for actually reading the issue body when it's
// taller than the visible window.
func TestDetail_Scroll_PageDownScrollsBodyEvenWithActivityRows(t *testing.T) {
	dm := detailFixture() // has comments + events + links
	km := newKeymap()
	if dm.scroll != 0 {
		t.Fatalf("setup: scroll = %d, want 0", dm.scroll)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgDown}, km, nil)
	if dm.scroll == 0 {
		t.Fatalf("pgdown did not scroll body; scroll = %d, want >0", dm.scroll)
	}
	prev := dm.scroll
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp}, km, nil)
	if dm.scroll >= prev {
		t.Fatalf("pgup did not unwind body scroll; scroll = %d, prev = %d", dm.scroll, prev)
	}
}

// TestDetail_Scroll_PageUpClampsAtTop: pgup at the top of the body is
// a no-op, not a negative scroll.
func TestDetail_Scroll_PageUpClampsAtTop(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp}, km, nil)
	if dm.scroll != 0 {
		t.Fatalf("scroll = %d, want 0 (clamped at top)", dm.scroll)
	}
}

// TestDetail_Scroll_PageDownWorksOnChildrenFocus: pgup/pgdn target
// the body regardless of which section the user is focused on, so a
// long body can be skimmed even while the cursor is parked on the
// children list.
func TestDetail_Scroll_PageDownWorksOnChildrenFocus(t *testing.T) {
	dm := detailFixture()
	dm.children = []Issue{{UID: "01TEST-99zz", ShortID: "99zz", Title: "child", Status: "open"}}
	dm.detailFocus = focusChildren
	km := newKeymap()
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgDown}, km, nil)
	if dm.scroll == 0 {
		t.Fatalf("pgdown on children focus did not scroll body; scroll = %d", dm.scroll)
	}
}

// TestDetail_Scroll_PageDownClampsPastEOF / PageUpAfterOverscroll:
// regression for roborev #17184. Without the clamp, repeated PgDn
// past EOF lets dm.scroll grow unbounded; the renderer hides this
// with its own per-frame clamp, but a follow-up PgUp then needs
// many presses to bring scroll back into the visible range,
// appearing stuck. The clamp keeps dm.scroll bounded so a single
// PgUp always produces a visible movement.
func TestDetail_Scroll_PageDownClampsPastEOFAndPageUpResponds(t *testing.T) {
	dm := detailFixture()
	dm.lastTermWidth, dm.lastTermHeight = 120, 30
	km := newKeymap()
	// The fixture's body is short, so any PgDn lands past the
	// renderer's max start. Press PgDn 12 times — without a clamp,
	// dm.scroll would be 96; with the clamp it stays at the body's
	// approximate maxStart (<= a small number for short bodies).
	for i := 0; i < 12; i++ {
		dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgDown}, km, nil)
	}
	if dm.scroll > 24 {
		t.Fatalf("PgDn past EOF should clamp dm.scroll, got scroll=%d", dm.scroll)
	}
	// One PgUp must produce a visible (smaller) scroll value.
	prev := dm.scroll
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp}, km, nil)
	if dm.scroll >= prev && prev > 0 {
		t.Fatalf("PgUp after overscroll did not respond visually: scroll=%d, prev=%d", dm.scroll, prev)
	}
}

// TestDetail_Scroll_PageDownClampsToDocument: a long body + some
// activity rows yields a document strictly taller than a 30-row
// terminal. Aggressive PgDn must clamp dm.scroll at the document's
// max-start so the renderer never windows past EOF — and so a single
// PgUp produces visible movement, not a no-op while an inflated
// offset unwinds (regression for roborev #17184, retargeted at the
// unified viewport).
func TestDetail_Scroll_PageDownClampsToDocument(t *testing.T) {
	dm := detailFixture()
	dm.issue.Body = strings.Repeat("line\n", 39) + "tail"
	width, height := 120, 30
	dm.lastTermWidth, dm.lastTermHeight = width, height
	docLines, _ := dm.detailDocumentLines(width, dm.scrollChrome())
	_, visible, _ := dm.viewportDims()
	maxStart := viewportMaxStart(len(docLines), visible)

	km := newKeymap()
	for i := 0; i < 12; i++ {
		dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgDown}, km, nil)
	}
	if dm.scroll > maxStart {
		t.Fatalf("PgDn past EOF: scroll=%d > maxStart=%d", dm.scroll, maxStart)
	}
	prev := dm.scroll
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgUp}, km, nil)
	if prev > 0 && dm.scroll >= prev {
		t.Fatalf("PgUp after overscroll did not respond: scroll=%d, prev=%d",
			dm.scroll, prev)
	}
}

// TestDetail_Scroll_SplitViewportClamp: in split-pane mode the cached
// lastDetailWidth/Height is the inner pane size. Aggressive PgDn must
// clamp scroll using that pane height, not the full terminal — split
// panes are typically much shorter and would otherwise let dm.scroll
// run far past the rendered EOF.
func TestDetail_Scroll_SplitViewportClamp(t *testing.T) {
	dm := detailFixture()
	dm.issue.Body = strings.Repeat("line\n", 59) + "tail"
	width, height := 72, 24
	dm.lastDetailWidth, dm.lastDetailHeight = width, height
	dm.lastDetailSplit = true
	docLines, _ := dm.detailDocumentLines(width, dm.scrollChrome())
	maxStart := viewportMaxStart(len(docLines), height)

	km := newKeymap()
	for i := 0; i < 30; i++ {
		dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyPgDown}, km, nil)
	}
	if dm.scroll != maxStart {
		t.Fatalf("PgDn in split overflowed: scroll=%d, want maxStart=%d",
			dm.scroll, maxStart)
	}
}

// TestDetail_JK_OnEmptyActivityTab_ScrollsViewport pins the regression
// in handleNavKey: when the focused activity tab has no rows
// (placeholder loading / error / empty), j/k must spill to viewport
// scroll without revealCursor immediately pinning the activity header
// back into view. Without this, scrolling near the bottom of a long
// body with focusActivity on an empty tab would snap the viewport
// back to the activity header on every press.
func TestDetail_JK_OnEmptyActivityTab_ScrollsViewport(t *testing.T) {
	dm := detailFixture()
	dm.issue.Body = strings.Repeat("body line\n", 60) + "tail"
	dm.links = nil // empty links tab; comments+events keep hasActivity true
	dm.activeTab = tabLinks
	dm.detailFocus = focusActivity
	dm.tabExplicit = true
	dm.lastTermWidth, dm.lastTermHeight = 100, 24

	// Scroll into the body so the activity header is well above the
	// viewport. Without the fix, revealCursor will pull scroll back to
	// the activity header on every j/k press.
	dm.scroll = 30
	docTotal, _ := dm.detailDocumentLines(dm.lastTermWidth, dm.scrollChrome())
	_, visible, ok := dm.viewportDims()
	if !ok || dm.scroll+visible >= len(docTotal) {
		t.Fatalf("setup: scroll=%d not deep into the document (visible=%d total=%d)",
			dm.scroll, visible, len(docTotal))
	}

	km := newKeymap()
	before := dm.scroll
	next, _ := dm.Update(runeKey('j'), km, nil)
	if next.scroll != before+1 {
		t.Fatalf("j on empty activity tab: scroll = %d, want %d — revealCursor likely pulled scroll back to the activity header",
			next.scroll, before+1)
	}
	next, _ = next.Update(runeKey('k'), km, nil)
	if next.scroll != before {
		t.Fatalf("k on empty activity tab: scroll = %d, want %d (before-1)",
			next.scroll, before)
	}
}

// TestDetail_OpenFromList_SeedsViewportCache pins the regression:
// the freshly-installed detail model in handleOpenDetail must inherit
// the terminal dimensions, not start from zero. Without the seed,
// viewportDims() returns ok=false, pageStep falls back to
// detailFallbackPageStep (8) regardless of how tall the terminal is,
// and scrollViewportBy skips its EOF clamp until the next
// WindowSizeMsg arrives.
func TestDetail_OpenFromList_SeedsViewportCache(t *testing.T) {
	m := scenarioModel(t, 200, 40) // split layout breakpoint is 140x36
	iss := listFixture()[0]
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)

	if m.detail.lastTermWidth != 200 || m.detail.lastTermHeight != 40 {
		t.Fatalf("lastTerm = %dx%d, want 200x40 after open",
			m.detail.lastTermWidth, m.detail.lastTermHeight)
	}
	if !m.detail.lastDetailSplit {
		t.Fatalf("lastDetailSplit = false at split layout 200x40")
	}
	_, visible, ok := m.detail.viewportDims()
	if !ok {
		t.Fatalf("viewportDims().ok = false after open; pageStep would be %d",
			m.detail.pageStep())
	}
	if step := m.detail.pageStep(); step != visible-detailPageOverlap {
		t.Errorf("pageStep = %d, want %d (visible-overlap)",
			step, visible-detailPageOverlap)
	}
}

// TestModel_ToggleLayout_RefreshesViewportCache pins the regression:
// pressing the layout-toggle key changes the detail pane's effective
// size (full-width stacked vs boxed split-pane inner area). Without
// refreshing the cache the next PgUp/PgDn keeps using the pre-toggle
// dimensions — overshooting after stacked → split (the split pane is
// smaller) or leaving slack after split → stacked.
func TestModel_ToggleLayout_RefreshesViewportCache(t *testing.T) {
	m := scenarioModel(t, 200, 40) // starts in split
	iss := listFixture()[0]
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if !m.detail.lastDetailSplit {
		t.Fatalf("setup: expected split layout cached")
	}

	m, _ = m.toggleLayout() // split → stacked
	if m.detail.lastDetailSplit {
		t.Fatalf("after toggle: lastDetailSplit still true; cache is stale")
	}
	if m.detail.lastTermWidth != 200 || m.detail.lastTermHeight != 40 {
		t.Fatalf("after toggle: lastTerm = %dx%d, want preserved 200x40",
			m.detail.lastTermWidth, m.detail.lastTermHeight)
	}
	if _, _, ok := m.detail.viewportDims(); !ok {
		t.Fatalf("viewportDims().ok = false after toggle")
	}
}

// TestDetail_Back_EmitsPopMsg: esc returns a tea.Cmd that emits
// popDetailMsg when the nav stack is empty.
func TestDetail_Back_EmitsPopMsg(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()
	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEsc}, km, nil)
	if cmd == nil {
		t.Fatal("esc must return a tea.Cmd")
	}
	msg := cmd()
	if _, ok := msg.(popDetailMsg); !ok {
		t.Fatalf("expected popDetailMsg, got %T", msg)
	}
}

// TestDetail_OpenFromList_DispatchesBatch: pressing Enter on a list row
// switches the model to viewDetail, seeds m.detail.issue, and returns
// a tea.Cmd. (We can't introspect a tea.Batch directly without running
// it, but we can verify the model state mutated correctly.)
func TestDetail_OpenFromList_DispatchesBatch(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	m.connGen = 9
	m.list.loading = false
	m.list.issues = []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "first"},
		{ProjectID: 7, UID: "01TEST-bbb2", ShortID: "bbb2", Title: "second"},
	}
	out, _ := m.Update(runeKey('j'))
	m = out.(Model)
	out, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = out.(Model)
	if cmd == nil {
		t.Fatal("expected open cmd from Enter")
	}
	msg := cmd()
	open, ok := msg.(openDetailMsg)
	if !ok {
		t.Fatalf("expected openDetailMsg, got %T", msg)
	}
	if open.issue.ShortID != "bbb2" {
		t.Fatalf("issue.ShortID = %q, want bbb2", open.issue.ShortID)
	}
	if open.connGen != 9 {
		t.Fatalf("openDetailMsg.connGen = %d, want 9", open.connGen)
	}
	out, _ = m.Update(open)
	m = out.(Model)
	if m.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail", m.view)
	}
	if m.detail.issue == nil || m.detail.issue.ShortID != "bbb2" {
		t.Fatalf("detail.issue not seeded correctly: %+v", m.detail.issue)
	}
	if m.detail.scopePID != 7 {
		t.Fatalf("detail.scopePID = %d, want 7", m.detail.scopePID)
	}
}

// TestDetail_PopReturnsToListPreservingState: detail → esc → list keeps
// the list cursor and filter state intact.
func TestDetail_PopReturnsToListPreservingState(t *testing.T) {
	m := initialModel(Options{})
	m.scope = scope{projectID: 7}
	m.list.loading = false
	m.list.cursor = 1
	m.list.filter = ListFilter{Status: "open", Search: "bug"}
	m.list.issues = []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "first bug"},
		{ProjectID: 7, UID: "01TEST-bbb2", ShortID: "bbb2", Title: "second bug"},
	}
	m.view = viewDetail
	m.detail.issue = &m.list.issues[1]
	out, _ := m.Update(popDetailMsg{})
	m = out.(Model)
	if m.view != viewList {
		t.Fatalf("view = %v, want viewList after pop", m.view)
	}
	if m.list.cursor != 1 {
		t.Fatalf("list cursor = %d, want 1 (preserved)", m.list.cursor)
	}
	if m.list.filter.Status != "open" || m.list.filter.Search != "bug" {
		t.Fatalf("list filter clobbered: %+v", m.list.filter)
	}
}

// TestDetail_Loading_Renders shows the loading hint while issue is nil.
func TestDetail_Loading_Renders(t *testing.T) {
	dm := detailModel{loading: true}
	out := dm.View(80, 24, viewChrome{})
	if !strings.Contains(out, "loading") {
		t.Fatalf("expected loading hint, got:\n%s", out)
	}
}

// TestDetail_FetchedMsgs_Populate: the three tab fetch messages seed
// the corresponding slices on dm.
func TestDetail_FetchedMsgs_Populate(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1"}}
	km := newKeymap()
	dm, _ = dm.Update(commentsFetchedMsg{
		comments: []CommentEntry{{ID: 1, Author: "a", Body: "x"}},
	}, km, nil)
	if len(dm.comments) != 1 {
		t.Fatalf("comments not populated: %+v", dm.comments)
	}
	dm, _ = dm.Update(eventsFetchedMsg{
		events: []EventLogEntry{{ID: 1, Type: "issue.created"}},
	}, km, nil)
	if len(dm.events) != 1 {
		t.Fatalf("events not populated: %+v", dm.events)
	}
	dm, _ = dm.Update(linksFetchedMsg{
		links: []LinkEntry{{ID: 1, Type: "blocks"}},
	}, km, nil)
	if len(dm.links) != 1 {
		t.Fatalf("links not populated: %+v", dm.links)
	}
}

// TestDetail_FetchedMsgs_ErrorRecorded: an error on a tab fetch
// surfaces on the per-tab error slot (dm.commentsErr in this case).
// The detail-issue dm.err remains a separate signal because it gates
// the entire view rather than one tab.
func TestDetail_FetchedMsgs_ErrorRecorded(t *testing.T) {
	dm := detailModel{issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1"}}
	km := newKeymap()
	dm, _ = dm.Update(commentsFetchedMsg{err: errors.New("boom")}, km, nil)
	if dm.commentsErr == nil || dm.commentsErr.Error() != "boom" {
		t.Fatalf("commentsErr = %v, want boom", dm.commentsErr)
	}
}

// TestDetail_TabPlaceholder_LoadingRendered: when a tab is marked
// loading, the renderer substitutes "(loading…)" for the entry list
// regardless of whether data is present.
func TestDetail_TabPlaceholder_LoadingRendered(t *testing.T) {
	dm := detailFixture()
	dm.commentsLoading = true
	lines, _ := dm.detailDocumentLines(80, viewChrome{})
	out := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(out, "loading") {
		t.Fatalf("expected loading placeholder on comments tab, got:\n%s", out)
	}
}

// TestDetail_TabPlaceholder_ErrorRendered: a per-tab error renders
// "comments: <err>" (style is theme-dependent so we just assert the
// substring) so the user can tell which tab failed.
func TestDetail_TabPlaceholder_ErrorRendered(t *testing.T) {
	dm := detailFixture()
	dm.commentsLoading = false
	dm.commentsErr = errors.New("server down")
	lines, _ := dm.detailDocumentLines(80, viewChrome{})
	out := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(out, "comments: server down") {
		t.Fatalf("expected per-tab error hint, got:\n%s", out)
	}
}

// TestHandleOpenDetail_DispatchesFetchIssue: opening a detail issue via
// openDetailMsg must dispatch all four detail fetches (issue + comments
// + events + links). handleJumpDetail already does this; the open path
// was missing the issue fetch, so the seeded list-row Issue (no Labels)
// stuck around until a manual refresh. Asserted at the batch shape so
// the children aren't actually run against the real *Client.
func TestHandleOpenDetail_DispatchesFetchIssue(t *testing.T) {
	m := initialModel(Options{})
	m.api = NewClient("http://kata.invalid", nil)
	m.scope = scope{projectID: 7}
	iss := Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "x"}
	_, cmd := m.Update(openDetailMsg{issue: iss})
	if cmd == nil {
		t.Fatal("expected batch cmd from openDetailMsg")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("expected tea.BatchMsg, got %T", cmd())
	}
	if got := len(batch); got != 4 {
		t.Fatalf("expected 4 fetches in open batch (issue + 3 tabs), got %d", got)
	}
}

// TestDetailFetch_PopulatesIssueLabelsOnOpen: the show-response carries
// labels (commit 1: showIssue lifts them onto resp.Issue.Labels), and
// applyFetched replaces dm.issue wholesale on detailFetchedMsg, so the
// labels arrive on dm.issue.Labels for the renderer to chip out. This
// pins the hard invariant: detail opens populate labels via the
// existing fetchIssue helper without any per-field copy.
func TestDetailFetch_PopulatesIssueLabelsOnOpen(t *testing.T) {
	dm := detailModel{
		gen:   1,
		issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "seed (no labels)"},
	}
	msg := detailFetchedMsg{
		gen: 1,
		issue: &Issue{
			UID:     "01TEST-42aa",
			ShortID: "42aa",
			Title:   "fetched",
			Labels:  []string{"bug"},
		},
	}
	out := dm.applyFetched(msg)
	if out.issue == nil {
		t.Fatal("dm.issue should not be nil after applyFetched")
	}
	if got, want := out.issue.Labels, []string{"bug"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("dm.issue.Labels = %v, want %v", got, want)
	}
}

func TestDetailApplyFetched_PopulatesParentAndChildren(t *testing.T) {
	dm := detailModel{gen: 1}
	msg := detailFetchedMsg{
		gen:      1,
		issue:    &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "parented"},
		parent:   &IssueRef{UID: "01TEST-7bb", ShortID: "7bb", Title: "workspace", Status: "open"},
		children: []Issue{{UID: "01TEST-43cc", ShortID: "43cc", Title: "child"}},
	}

	out := dm.applyFetched(msg)
	if out.parent == nil || out.parent.ShortID != "7bb" {
		t.Fatalf("parent = %+v, want #7bb", out.parent)
	}
	if len(out.children) != 1 || out.children[0].ShortID != "43cc" {
		t.Fatalf("children = %+v, want #43cc", out.children)
	}
}

// TestDetail_OpenDetail_SeedsLoadingFlags: opening detail through the
// model-level handler seeds all three per-tab loading flags so the
// initial render shows "(loading…)" until the tab fetches return.
func TestDetail_OpenDetail_SeedsLoadingFlags(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	iss := Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if !m.detail.commentsLoading {
		t.Fatal("commentsLoading should be true after open")
	}
	if !m.detail.eventsLoading {
		t.Fatal("eventsLoading should be true after open")
	}
	if !m.detail.linksLoading {
		t.Fatal("linksLoading should be true after open")
	}
}

// TestDetail_ProjectID_AllProjectsUsesIssueProjectID: in all-projects
// scope, detailProjectID prefers the issue's ProjectID field over the
// (zero) sc.projectID so the URL is correct.
func TestDetail_ProjectID_AllProjectsUsesIssueProjectID(t *testing.T) {
	iss := Issue{ProjectID: 42, UID: "01TEST-aaa1", ShortID: "aaa1"}
	got := detailProjectID(iss, scope{allProjects: true})
	if got != 42 {
		t.Fatalf("detailProjectID = %d, want 42", got)
	}
}

// TestDetail_ProjectID_SingleProjectUsesScope: in single-project scope,
// detailProjectID always uses sc.projectID even when the issue carries
// its own (they should match anyway).
func TestDetail_ProjectID_SingleProjectUsesScope(t *testing.T) {
	iss := Issue{ProjectID: 99, UID: "01TEST-aaa1", ShortID: "aaa1"}
	got := detailProjectID(iss, scope{projectID: 7})
	if got != 7 {
		t.Fatalf("detailProjectID = %d, want 7", got)
	}
}

// TestDetail_HardWrap covers the body-line wrapper.
func TestDetail_HardWrap(t *testing.T) {
	got := hardWrap("abcdefghij", 4)
	want := []string{"abcd", "efgh", "ij"}
	if len(got) != len(want) {
		t.Fatalf("got %d chunks, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("chunk %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDetail_HardWrap_OversizeRune confirms the wrapper makes progress
// when the leading rune is wider than the requested width.
func TestDetail_HardWrap_OversizeRune(t *testing.T) {
	got := hardWrap("你好世界", 1)
	if len(got) != 4 {
		t.Fatalf("expected 4 chunks, got %d: %v", len(got), got)
	}
}

// fakeDetailAPI is the test double for detailAPI used by the fetch-cmd
// tests and the jump-nav tests. The exported result fields seed the
// return values; lastGetIssue captures the most recent GetIssueDetail call so
// jump tests can assert on the issue number that was fetched. The
// mutation counters/last-* fields support the Task 9 mutation tests.
type fakeDetailAPI struct {
	commentsResult []CommentEntry
	eventsResult   []EventLogEntry
	linksResult    []LinkEntry
	commentsErr    error
	eventsErr      error
	linksErr       error

	getIssueResult *Issue
	getIssueErr    error
	lastGetIssue   string

	closeCalls       int
	reopenCalls      int
	addLabelCalls    int
	removeLabelCalls int
	assignCalls      int
	addLinkCalls     int
	editBodyCalls    int
	addCommentCalls  int
	setPriorityCalls int

	lastProjectID int64
	lastRef       string
	lastActor     string
	lastLabel     string
	lastOwner     string
	lastBody      string
	lastLinkBody  LinkBody
	lastPriority  *int64

	mutationResult *MutationResp
	mutationErr    error
}

func (f *fakeDetailAPI) GetIssueDetail(
	_ context.Context, _ int64, ref string,
) (*IssueDetail, error) {
	f.lastGetIssue = ref
	if f.getIssueResult == nil {
		return nil, f.getIssueErr
	}
	return &IssueDetail{Issue: f.getIssueResult}, f.getIssueErr
}

func (f *fakeDetailAPI) ListComments(
	_ context.Context, _ int64, _ string,
) ([]CommentEntry, error) {
	return f.commentsResult, f.commentsErr
}

func (f *fakeDetailAPI) ListEvents(
	_ context.Context, _ int64, _ string,
) ([]EventLogEntry, error) {
	return f.eventsResult, f.eventsErr
}

func (f *fakeDetailAPI) ListLinks(
	_ context.Context, _ int64, _ string,
) ([]LinkEntry, error) {
	return f.linksResult, f.linksErr
}

func (f *fakeDetailAPI) Close(
	_ context.Context, projectID int64, ref, actor string,
) (*MutationResp, error) {
	f.closeCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) Reopen(
	_ context.Context, projectID int64, ref, actor string,
) (*MutationResp, error) {
	f.reopenCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) AddLabel(
	_ context.Context, projectID int64, ref, label, actor string,
) (*MutationResp, error) {
	f.addLabelCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastLabel = label
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) RemoveLabel(
	_ context.Context, projectID int64, ref, label, actor string,
) (*MutationResp, error) {
	f.removeLabelCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastLabel = label
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) Assign(
	_ context.Context, projectID int64, ref, owner, actor string,
) (*MutationResp, error) {
	f.assignCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastOwner = owner
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) SetPriority(
	_ context.Context, projectID int64, ref string, priority *int64, actor string,
) (*MutationResp, error) {
	f.setPriorityCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastPriority = priority
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) AddLink(
	_ context.Context, projectID int64, ref string, body LinkBody, actor string,
) (*MutationResp, error) {
	f.addLinkCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastLinkBody = body
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) EditBody(
	_ context.Context, projectID int64, ref, body, actor string,
) (*MutationResp, error) {
	f.editBodyCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastBody = body
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

func (f *fakeDetailAPI) AddComment(
	_ context.Context, projectID int64, ref, body, actor string,
) (*MutationResp, error) {
	f.addCommentCalls++
	f.lastProjectID = projectID
	f.lastRef = ref
	f.lastBody = body
	f.lastActor = actor
	return f.mutationResult, f.mutationErr
}

// TestDetail_FetchCommands_RoundTrip exercises the three fetch wrappers
// through their tea.Cmd contracts.
func TestDetail_FetchCommands_RoundTrip(t *testing.T) {
	api := &fakeDetailAPI{
		commentsResult: []CommentEntry{{ID: 1, Author: "a"}},
		eventsResult:   []EventLogEntry{{ID: 2, Type: "issue.created"}},
		linksResult:    []LinkEntry{{ID: 3, Type: "blocks"}},
	}
	cm, ok := fetchComments(api, 7, "42aa", 1)().(commentsFetchedMsg)
	if !ok {
		t.Fatalf("expected commentsFetchedMsg")
	}
	if len(cm.comments) != 1 || cm.comments[0].Author != "a" {
		t.Fatalf("comments payload wrong: %+v", cm.comments)
	}
	if cm.gen != 1 {
		t.Fatalf("commentsFetchedMsg.gen = %d, want 1", cm.gen)
	}
	em, ok := fetchEvents(api, 7, "42aa", 1)().(eventsFetchedMsg)
	if !ok {
		t.Fatalf("expected eventsFetchedMsg")
	}
	if len(em.events) != 1 || em.events[0].Type != "issue.created" {
		t.Fatalf("events payload wrong: %+v", em.events)
	}
	lm, ok := fetchLinks(api, 7, "42aa", 1)().(linksFetchedMsg)
	if !ok {
		t.Fatalf("expected linksFetchedMsg")
	}
	if len(lm.links) != 1 || lm.links[0].Type != "blocks" {
		t.Fatalf("links payload wrong: %+v", lm.links)
	}
}

// TestDetail_Viewport_ScrollAdvancesWindow: with dm.scroll = N the
// viewport's first visible row is documentLines[N], so the user reads
// later content as the offset grows.
func TestDetail_Viewport_ScrollAdvancesWindow(t *testing.T) {
	dm := detailFixture()
	docLines, _ := dm.detailDocumentLines(120, viewChrome{})
	want := stripANSI(docLines[5])
	dm.lastTermWidth, dm.lastTermHeight = 120, 30
	dm.scroll = 5
	out := stripANSI(dm.View(120, 30, viewChrome{}))
	// Title bar + blank are pinned chrome (lines 0-1); the first
	// document row sits on line 2.
	rows := strings.Split(out, "\n")
	if len(rows) < 3 {
		t.Fatalf("view too short:\n%s", out)
	}
	if !strings.Contains(rows[2], strings.TrimSpace(want)) {
		t.Fatalf("first visible row mismatch:\n want %q\n  got %q", want, rows[2])
	}
}

// TestDetail_Viewport_ScrollClampsAtEOF: an out-of-range dm.scroll is
// clamped against viewportMaxStart so the renderer never produces a
// window of blanks past the last line.
func TestDetail_Viewport_ScrollClampsAtEOF(t *testing.T) {
	dm := detailFixture()
	dm.scroll = 10000
	dm.lastTermWidth, dm.lastTermHeight = 120, 30
	out := stripANSI(dm.View(120, 30, viewChrome{}))
	if !strings.Contains(out, "tail") {
		t.Fatalf("expected tail of body near EOF, got:\n%s", out)
	}
}

// TestDetail_OpenInAllProjectsScope_UsesIssueProjectID: in all-projects
// mode, opening an issue dispatches fetches against the issue's own
// project_id (not the scope's, which is zero in all-projects mode).
func TestDetail_OpenInAllProjectsScope_UsesIssueProjectID(t *testing.T) {
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{allProjects: true}
	iss := Issue{ProjectID: 99, UID: "01TEST-eee5", ShortID: "eee5", Title: "cross-project"}
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if m.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail", m.view)
	}
	if m.detail.issue == nil || m.detail.issue.ProjectID != 99 {
		t.Fatalf("detail.issue not seeded correctly: %+v", m.detail.issue)
	}
	if m.detail.scopePID != 99 {
		t.Fatalf("detail.scopePID = %d, want 99", m.detail.scopePID)
	}
}

// TestDetail_OpenWithNilAPI_NoCrash: without a wired client (test
// harness path), the open handler still seeds the model and returns
// nil instead of panicking on the fetch dispatch.
func TestDetail_OpenWithNilAPI_NoCrash(t *testing.T) {
	m := initialModel(Options{})
	m.api = nil
	out, cmd := m.Update(openDetailMsg{issue: Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}})
	if cmd != nil {
		t.Fatalf("expected nil cmd when api is nil, got %T", cmd)
	}
	m = out.(Model)
	if m.view != viewDetail {
		t.Fatalf("view = %v, want viewDetail", m.view)
	}
}

// TestDetail_TinyTerminal_RendersWithoutPanic: very small terminals
// fall through to the bare-render fallback so the user sees something
// rather than a panic from negative budgets. After M3.5 the body+tab
// split is computed inline in View; the floors detailMinBodyRows and
// detailMinTabRows still cap the inner allocations.
func TestDetail_TinyTerminal_RendersWithoutPanic(t *testing.T) {
	dm := detailFixture()
	out := dm.View(80, 4, viewChrome{})
	if out == "" {
		t.Fatal("tiny render must produce some output")
	}
}

// TestDetail_RenderCommentsTab_FormatsAuthorAndIndentsBody confirms the
// per-comment header uses an author + compact timestamp and body lines
// are indented by 2 spaces under the header.
func TestDetail_RenderCommentsTab_FormatsAuthorAndIndentsBody(t *testing.T) {
	cs := []CommentEntry{
		{
			ID: 1, Author: "alice",
			Body:      "hello there\nthis is line two",
			CreatedAt: time.Date(2025, 1, 2, 15, 4, 0, 0, time.UTC),
		},
	}
	out := renderCommentsTab(cs, 80, 20, -1, tabState{})
	if !strings.Contains(out, "alice  Jan 2 15:04") {
		t.Fatalf("missing author/timestamp header:\n%s", out)
	}
	if !strings.Contains(out, "  hello there") {
		t.Fatalf("body line not indented:\n%s", out)
	}
	if !strings.Contains(out, "  this is line two") {
		t.Fatalf("multi-line body not indented:\n%s", out)
	}
}

// TestDetail_RenderCommentsTab_EmptyShowsHint shows the placeholder when
// there are no comments. The "Comments (N)" header is now rendered by
// the tab strip at the detail-view level (see renderTabStrip), so the
// placeholder body is the only thing this renderer produces for the
// zero-comments case.
func TestDetail_RenderCommentsTab_EmptyShowsHint(t *testing.T) {
	out := renderCommentsTab(nil, 80, 5, -1, tabState{})
	if !strings.Contains(out, "no comments") {
		t.Fatalf("expected placeholder, got:\n%s", out)
	}
}

// TestDetail_RenderEventsTab_FormatsCommonEventTypes covers a slice
// over the type vocabulary so the description column is in lockstep.
func TestDetail_RenderEventsTab_FormatsCommonEventTypes(t *testing.T) {
	when := time.Date(2025, 1, 2, 15, 4, 0, 0, time.UTC)
	toShort := "k1l1"
	es := []EventLogEntry{
		{Type: "issue.created", Actor: "a", CreatedAt: when},
		{Type: "issue.closed", Actor: "a", CreatedAt: when,
			Payload: map[string]any{"reason": "wontfix"}},
		{Type: "issue.labeled", Actor: "b", CreatedAt: when,
			Payload: map[string]any{"label": "bug"}},
		{Type: "issue.linked", Actor: "c", CreatedAt: when,
			IssueShortID: &toShort,
			Payload: map[string]any{
				"type": "blocks", "to_short_id": "k1l1",
			}},
		{Type: "issue.assigned", Actor: "d", CreatedAt: when,
			Payload: map[string]any{"owner": "wesm"}},
	}
	out := renderEventsTab(es, 200, 20, -1, tabState{})
	assertContainsAll(t, out,
		"[issue.created] 2025-01-02 15:04 a — created",
		"[issue.closed] 2025-01-02 15:04 a — closed (wontfix)",
		"[issue.labeled] 2025-01-02 15:04 b — labeled bug",
		"[issue.linked] 2025-01-02 15:04 c — linked blocks #k1l1",
		"[issue.assigned] 2025-01-02 15:04 d — assigned wesm",
	)
}

// TestDetail_RenderEventsTab_UnknownTypeFallback: an unrecognized event
// type renders with the "issue." prefix stripped so the row still says
// something coherent.
func TestDetail_RenderEventsTab_UnknownTypeFallback(t *testing.T) {
	es := []EventLogEntry{{Type: "issue.future_thing", Actor: "a"}}
	out := renderEventsTab(es, 80, 5, -1, tabState{})
	if !strings.Contains(out, "future_thing") {
		t.Fatalf("expected fallback description:\n%s", out)
	}
}

// TestDetail_RenderLinksTab_FormatsLinkLine confirms the link line shape.
func TestDetail_RenderLinksTab_FormatsLinkLine(t *testing.T) {
	when := time.Date(2025, 1, 2, 15, 4, 0, 0, time.UTC)
	ls := []LinkEntry{
		{
			ID: 1, Type: "blocks",
			From:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
			To:     LinkPeer{UID: "01TEST-7bb", ShortID: "7bb"},
			Author: "wesm", CreatedAt: when,
		},
	}
	out := renderLinksTab(ls, 200, 5, -1, tabState{})
	want := "[blocks] → #7bb ← #42aa  by wesm @ 2025-01-02 15:04"
	if !strings.Contains(out, want) {
		t.Fatalf("missing link line %q in:\n%s", want, out)
	}
}

// TestDetail_RenderLinksTab_EmptyShowsHint shows the placeholder when
// there are no links.
func TestDetail_RenderLinksTab_EmptyShowsHint(t *testing.T) {
	out := renderLinksTab(nil, 80, 5, -1, tabState{})
	if !strings.Contains(out, "no links") {
		t.Fatalf("expected placeholder, got:\n%s", out)
	}
}

// TestDetail_TabCursor_MovesWithJK: on a tab with rows, j/k moves the
// tab cursor (not the body scroll).
func TestDetail_TabCursor_MovesWithJK(t *testing.T) {
	dm := detailFixture() // 2 comments
	km := newKeymap()
	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.tabCursor != 1 {
		t.Fatalf("after j: tabCursor = %d, want 1", dm.tabCursor)
	}
	if dm.scroll != 0 {
		t.Fatalf("after j: scroll = %d, want 0 (j on tab moves cursor)", dm.scroll)
	}
	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.tabCursor != 1 { // clamped at len-1
		t.Fatalf("after second j: tabCursor = %d, want 1 (clamped)", dm.tabCursor)
	}
	dm, _ = dm.Update(runeKey('k'), km, nil)
	if dm.tabCursor != 0 {
		t.Fatalf("after k: tabCursor = %d, want 0", dm.tabCursor)
	}
}

// TestDetail_TabSwitch_ResetsCursor: switching tabs resets the row
// cursor so a stale index doesn't carry over to a different-length tab.
func TestDetail_TabSwitch_ResetsCursor(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()
	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.tabCursor != 1 {
		t.Fatalf("setup: tabCursor = %d, want 1", dm.tabCursor)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.tabCursor != 0 {
		t.Fatalf("after tab switch: tabCursor = %d, want 0", dm.tabCursor)
	}
}

func TestDetailFocus_TabCyclesChildrenCommentsEventsLinks(t *testing.T) {
	dm := detailFixture()
	dm.children = []Issue{{UID: "01TEST-43cc", ShortID: "43cc"}}
	km := newKeymap()

	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyShiftTab}, km, nil)
	if dm.detailFocus != focusChildren {
		t.Fatalf("shift+tab from comments focus = %v, want children", dm.detailFocus)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.detailFocus != focusActivity || dm.activeTab != tabComments {
		t.Fatalf("tab from children focus/tab = %v/%v, want activity/comments", dm.detailFocus, dm.activeTab)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.activeTab != tabEvents {
		t.Fatalf("tab from comments activeTab = %v, want events", dm.activeTab)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.activeTab != tabLinks {
		t.Fatalf("tab from events activeTab = %v, want links", dm.activeTab)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.detailFocus != focusChildren {
		t.Fatalf("tab from links focus = %v, want children", dm.detailFocus)
	}
}

func TestDetailFocus_SkipsChildrenWhenEmpty(t *testing.T) {
	dm := detailFixture()
	km := newKeymap()

	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyTab}, km, nil)
	if dm.detailFocus != focusActivity || dm.activeTab != tabEvents {
		t.Fatalf("tab with no children focus/tab = %v/%v, want activity/events", dm.detailFocus, dm.activeTab)
	}
	dm, _ = dm.Update(tea.KeyMsg{Type: tea.KeyShiftTab}, km, nil)
	if dm.detailFocus != focusActivity || dm.activeTab != tabComments {
		t.Fatalf("shift+tab with no children focus/tab = %v/%v, want activity/comments", dm.detailFocus, dm.activeTab)
	}
}

func TestDetailChildren_JKMovesChildCursor(t *testing.T) {
	dm := detailFixture()
	dm.children = []Issue{{UID: "01TEST-43cc", ShortID: "43cc"}, {UID: "01TEST-44dd", ShortID: "44dd"}}
	dm.detailFocus = focusChildren
	km := newKeymap()

	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.childCursor != 1 {
		t.Fatalf("after j childCursor = %d, want 1", dm.childCursor)
	}
	dm, _ = dm.Update(runeKey('j'), km, nil)
	if dm.childCursor != 1 {
		t.Fatalf("after second j childCursor = %d, want clamp 1", dm.childCursor)
	}
	dm, _ = dm.Update(runeKey('k'), km, nil)
	if dm.childCursor != 0 {
		t.Fatalf("after k childCursor = %d, want 0", dm.childCursor)
	}
}

func TestDetailChildren_EnterJumpsToChild(t *testing.T) {
	dm := detailFixture()
	dm.children = []Issue{{UID: "01TEST-43cc", ShortID: "43cc"}, {UID: "01TEST-44dd", ShortID: "44dd"}}
	dm.detailFocus = focusChildren
	dm.childCursor = 1
	km := newKeymap()

	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, &fakeDetailAPI{})
	assertJumpDetailCmd(t, cmd, "44dd")
}

// runBatch unwraps a tea.Batch wrapper and runs every nested cmd in
// sequence so jump-nav tests can observe the side effects of fetchIssue.
// We deliberately ignore tea.Cmd return values from sub-cmds because the
// fake API records what we want to assert.
func runBatch(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return
	}
	for _, sub := range batch {
		if sub != nil {
			_ = sub()
		}
	}
}

// TestDetail_EnterOnEventWithIssueRef_JumpsAndStacks: pressing Enter on
// a link event whose payload carries to_number emits a jumpDetailMsg
// with that number. The actual navStack push and fetch dispatch happen
// at the Model level (Model.handleJumpDetail) so the new gen comes
// from the monotonic m.nextGen counter.
func TestDetail_EnterOnEventWithIssueRef_JumpsAndStacks(t *testing.T) {
	api := &fakeDetailAPI{
		getIssueResult: &Issue{UID: "01TEST-l1l1", ShortID: "l1l1", Title: "linked target"},
	}
	dm := detailFixture()
	dm.activeTab = tabEvents
	dm.events = []EventLogEntry{
		{Type: "issue.linked", Actor: "wesm",
			Payload: map[string]any{
				"type": "blocks", "to_short_id": "l1l1",
			}},
	}
	dm.tabCursor = 0
	km := newKeymap()
	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	assertJumpDetailCmd(t, cmd, "l1l1")
}

func TestDetail_EnterOnCurrentIssueEventDoesNotSelfJump(t *testing.T) {
	api := &fakeDetailAPI{}
	dm := detailFixture()
	dm.activeTab = tabEvents
	dm.tabCursor = 0
	dm.events = []EventLogEntry{
		{Type: "issue.created", Actor: "wesm",
			Payload: map[string]any{"issue_short_id": dm.issue.ShortID}},
	}

	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, newKeymap(), api)
	if cmd != nil {
		t.Fatalf("Enter on current-issue event emitted jump cmd %T; want no-op", cmd)
	}
}

// TestDetail_EnterOnLinkEntry_JumpsToTarget: pressing Enter on a link
// row emits a jumpDetailMsg targeting the link's To.ShortID.
func TestDetail_EnterOnLinkEntry_JumpsToTarget(t *testing.T) {
	api := &fakeDetailAPI{
		getIssueResult: &Issue{UID: "01TEST-7bb", ShortID: "7bb", Title: "target"},
	}
	dm := detailFixture()
	dm.activeTab = tabLinks
	dm.tabCursor = 0
	km := newKeymap()
	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	assertJumpDetailCmd(t, cmd, "7bb")
}

// TestDetail_EnterOnIncomingLink_JumpsToFromShortID: when the cursor is
// on a link whose To matches the current issue (i.e. an incoming
// "X blocks me" entry), Enter must emit a jumpDetailMsg targeting
// From.ShortID rather than re-opening the current issue.
func TestDetail_EnterOnIncomingLink_JumpsToFromShortID(t *testing.T) {
	api := &fakeDetailAPI{
		getIssueResult: &Issue{UID: "01TEST-99zz", ShortID: "99zz", Title: "from"},
	}
	dm := detailModel{
		issue:     &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "current"},
		scopePID:  7,
		activeTab: tabLinks,
		tabCursor: 0,
		links: []LinkEntry{
			// Incoming: someone else (#99zz) blocks the current issue (#42aa).
			{
				ID: 1, Type: "blocks",
				From: LinkPeer{UID: "01TEST-99zz", ShortID: "99zz"},
				To:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
			},
		},
	}
	km := newKeymap()
	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	assertJumpDetailCmd(t, cmd, "99zz")
}

// TestLinkJumpTarget_OutgoingPicksTo: when To differs from the
// current issue, the helper returns the To short_id. Pure unit test
// for the scoping logic.
func TestLinkJumpTarget_OutgoingPicksTo(t *testing.T) {
	links := []LinkEntry{{
		ID: 1, Type: "blocks",
		From: LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
		To:   LinkPeer{UID: "01TEST-7bb", ShortID: "7bb"},
	}}
	got, ok := linkJumpTarget(links, 0, "42aa")
	if !ok || got != "7bb" {
		t.Fatalf("linkJumpTarget = (%q, %v), want (7bb, true)", got, ok)
	}
}

// TestLinkJumpTarget_IncomingPicksFrom: when To matches the current
// issue, the helper picks From.
func TestLinkJumpTarget_IncomingPicksFrom(t *testing.T) {
	links := []LinkEntry{{
		ID: 1, Type: "blocks",
		From: LinkPeer{UID: "01TEST-99zz", ShortID: "99zz"},
		To:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
	}}
	got, ok := linkJumpTarget(links, 0, "42aa")
	if !ok || got != "99zz" {
		t.Fatalf("linkJumpTarget = (%q, %v), want (99zz, true)", got, ok)
	}
}

// TestDetail_TabWindow_KeepsCursorVisible: a tabCursor past the visible
// height must still produce output that contains the cursor entry. The
// fixture has 10 events; a budget of 4 lines forces windowing.
func TestDetail_TabWindow_KeepsCursorVisible(t *testing.T) {
	events := make([]EventLogEntry, 10)
	for i := range events {
		events[i] = EventLogEntry{
			Type: "issue.commented", Actor: fmt.Sprintf("user-%d", i),
		}
	}
	out := renderEventsTab(events, 200, 5, 8, tabState{}) // header + 4 visible rows
	if !strings.Contains(out, "user-8") {
		t.Fatalf("cursor row (user-8) missing from windowed output:\n%s", out)
	}
	// Earlier behavior would have shown user-0..user-3; assert those are
	// gone so the window actually slid.
	if strings.Contains(out, "user-0 ") {
		t.Fatalf("non-windowed: user-0 still present despite cursor=8:\n%s", out)
	}
}

// TestDetail_TabWindow_NarrowFitsAll: a budget that fits every entry
// renders them without windowing — defensive against the windowing
// path firing when it shouldn't.
func TestDetail_TabWindow_NarrowFitsAll(t *testing.T) {
	events := []EventLogEntry{
		{Type: "issue.commented", Actor: "alice"},
		{Type: "issue.commented", Actor: "bob"},
	}
	out := renderEventsTab(events, 200, 10, 0, tabState{})
	if !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Fatalf("expected both rows, got:\n%s", out)
	}
}

// TestDetail_CommentsTabWindow_KeepsCursorVisible: comments produce
// multi-line chunks; the windower must still keep the cursor entry
// visible by sliding entry-by-entry.
func TestDetail_CommentsTabWindow_KeepsCursorVisible(t *testing.T) {
	cs := make([]CommentEntry, 6)
	for i := range cs {
		cs[i] = CommentEntry{
			ID: int64(i + 1), Author: fmt.Sprintf("user-%d", i),
			Body: fmt.Sprintf("body-%d", i),
		}
	}
	// Each comment chunk is ~3 lines (header, body, separator). Budget
	// of 6 → header + ~5 rows visible. Cursor at 5 (last) should appear.
	out := renderCommentsTab(cs, 200, 6, 5, tabState{})
	if !strings.Contains(out, "user-5") {
		t.Fatalf("cursor entry user-5 missing from windowed output:\n%s", out)
	}
	if strings.Contains(out, "user-0]") {
		t.Fatalf("non-windowed: user-0 still present despite cursor=5:\n%s", out)
	}
}

// TestDetail_LinksTabWindow_KeepsCursorVisible mirrors the events test
// for the links tab. Each link is one line, so the math is the same.
func TestDetail_LinksTabWindow_KeepsCursorVisible(t *testing.T) {
	ls := make([]LinkEntry, 10)
	for i := range ls {
		toSID := fmt.Sprintf("t%03d", i+1)
		ls[i] = LinkEntry{
			ID: int64(i + 1), Type: "blocks",
			From:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
			To:     LinkPeer{UID: "01TEST-" + toSID, ShortID: toSID},
			Author: fmt.Sprintf("u%d", i),
		}
	}
	out := renderLinksTab(ls, 200, 5, 8, tabState{})
	if !strings.Contains(out, "by u8") {
		t.Fatalf("cursor link (u8) missing:\n%s", out)
	}
}

// TestWindowChunks_AnchorsAtTopWhenCursorFits: cursor near 0 with a
// budget that fits a few entries leaves the slice anchored at the
// top — no spurious sliding.
func TestWindowChunks_AnchorsAtTopWhenCursorFits(t *testing.T) {
	chunks := []entryChunk{
		{lines: []string{"a"}}, {lines: []string{"b"}},
		{lines: []string{"c"}}, {lines: []string{"d"}},
	}
	got := windowChunks(chunks, 1, 2)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(got))
	}
	if got[0].lines[0] != "a" || got[1].lines[0] != "b" {
		t.Fatalf("expected [a b], got %+v", got)
	}
}

// TestDetail_EnterOnComment_NoJump: pressing Enter on a comment row
// does not jump (comments tab has no jump action).
func TestDetail_EnterOnComment_NoJump(t *testing.T) {
	api := &fakeDetailAPI{}
	dm := detailFixture() // active tab is tabComments
	km := newKeymap()
	dm, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	if cmd != nil {
		t.Fatalf("expected nil cmd from Enter on comment, got %T", cmd)
	}
	if len(dm.navStack) != 0 {
		t.Fatalf("navStack should be empty after no-op Enter, got %d", len(dm.navStack))
	}
}

// TestDetail_EscFromStackedDetail_PopsToPrior: Esc on a stacked detail
// pops the nav stack, restoring the prior detailModel verbatim.
func TestDetail_EscFromStackedDetail_PopsToPrior(t *testing.T) {
	prior := detailModel{
		issue:     &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "prior"},
		activeTab: tabEvents,
		tabCursor: 1,
	}
	current := detailModel{
		issue:    &Issue{UID: "01TEST-11bb", ShortID: "11bb", Title: "stacked"},
		navStack: []detailModel{prior},
	}
	km := newKeymap()
	got, cmd := current.Update(tea.KeyMsg{Type: tea.KeyEsc}, km, nil)
	if cmd != nil {
		t.Fatalf("expected nil cmd (no popDetailMsg), got %T", cmd)
	}
	if got.issue == nil || got.issue.ShortID != "42aa" {
		t.Fatalf("expected pop to issue #42aa, got %+v", got.issue)
	}
	if got.activeTab != tabEvents {
		t.Fatalf("activeTab not restored: got %d, want tabEvents", got.activeTab)
	}
	if got.tabCursor != 1 {
		t.Fatalf("tabCursor not restored: got %d, want 1", got.tabCursor)
	}
	if len(got.navStack) != 0 {
		t.Fatalf("navStack should be empty after pop, got %d", len(got.navStack))
	}
}

// TestDetail_EscFromTopLevelDetail_ReturnsToList: with an empty nav
// stack, Esc emits popDetailMsg as before.
func TestDetail_EscFromTopLevelDetail_ReturnsToList(t *testing.T) {
	m := newTestModel()
	m.connGen = 9
	m.view = viewDetail
	m.detail = detailFixture()
	_, cmd := updateModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("expected popDetailCmd")
	}
	msg, ok := cmd().(popDetailMsg)
	if !ok {
		t.Fatalf("expected popDetailMsg, got %T", cmd())
	}
	if msg.connGen != 9 {
		t.Fatalf("popDetailMsg.connGen = %d, want 9", msg.connGen)
	}
}

// TestDetail_NavStackCappedAtOne: trying to jump from a level-2 detail
// no-ops because the stack is at cap. Esc still pops as expected.
func TestDetail_NavStackCappedAtOne(t *testing.T) {
	api := &fakeDetailAPI{getIssueResult: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}}
	prior := detailModel{issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa"}, activeTab: tabLinks}
	dm := detailModel{
		issue:     &Issue{UID: "01TEST-11bb", ShortID: "11bb"},
		activeTab: tabLinks,
		links: []LinkEntry{{
			ID: 1, Type: "blocks",
			From: LinkPeer{UID: "01TEST-11bb", ShortID: "11bb"},
			To:   LinkPeer{UID: "01TEST-99zz", ShortID: "99zz"},
		}},
		navStack: []detailModel{prior}, // already at cap
	}
	km := newKeymap()
	dm, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	if cmd != nil {
		t.Fatalf("expected nil cmd at nav cap, got %T", cmd)
	}
	if dm.issue.ShortID != "11bb" {
		t.Fatalf("dm.issue should be unchanged, got %q", dm.issue.ShortID)
	}
	if len(dm.navStack) != 1 {
		t.Fatalf("navStack should still be at 1, got %d", len(dm.navStack))
	}
}

// TestDetail_EnterOnEventWithoutPayload_NoOp: an event whose payload
// has no to_number/issue_number is not jumpable.
func TestDetail_EnterOnEventWithoutPayload_NoOp(t *testing.T) {
	api := &fakeDetailAPI{getIssueResult: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1"}}
	dm := detailModel{
		issue:     &Issue{UID: "01TEST-11bb", ShortID: "11bb"},
		activeTab: tabEvents,
		events:    []EventLogEntry{{Type: "issue.created"}},
	}
	km := newKeymap()
	_, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	if cmd != nil {
		t.Fatalf("expected nil cmd for non-jumpable event, got %T", cmd)
	}
}

// TestDetail_ApplyActivityCursor_UsesTextMarker confirms activity rows
// use a NO_COLOR-visible marker rather than a painted row background.
func TestDetail_ApplyActivityCursor_UsesTextMarker(t *testing.T) {
	plain := applyActivityCursor("hello", false)
	if plain != "  hello" {
		t.Fatalf("non-cursor branch should indent text, got %q", plain)
	}
	styled := applyActivityCursor("hello", true)
	if styled != "> hello" {
		t.Fatalf("cursor branch should use marker: got %q", styled)
	}
}

// TestDetail_StaleFetch_DroppedAcrossOpen: open issue A → in-flight
// fetch for A lands after the user has popped and reopened B. The B
// view must not pick up A's comments/events/links/issue.
func TestDetail_StaleFetch_DroppedAcrossOpen(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	m.list.loading = false
	m.list.issues = []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "A"},
		{ProjectID: 7, UID: "01TEST-bbb2", ShortID: "bbb2", Title: "B"},
	}
	// Open A.
	out, _ := m.Update(openDetailMsg{issue: m.list.issues[0]})
	m = out.(Model)
	genA := m.detail.gen
	// Pop back to list.
	out, _ = m.Update(popDetailMsg{})
	m = out.(Model)
	// Open B — gen advances.
	out, _ = m.Update(openDetailMsg{issue: m.list.issues[1]})
	m = out.(Model)
	if m.detail.gen == genA {
		t.Fatal("gen should advance on second open")
	}
	// A stale fetch from A's gen lands.
	staleComments := commentsFetchedMsg{
		gen:      genA,
		comments: []CommentEntry{{ID: 99, Body: "from A"}},
	}
	out, _ = m.Update(staleComments)
	m = out.(Model)
	if len(m.detail.comments) != 0 {
		t.Fatalf("stale comments leaked into B view: %+v", m.detail.comments)
	}
	staleIssue := detailFetchedMsg{
		gen:   genA,
		issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "A clobbered"},
	}
	out, _ = m.Update(staleIssue)
	m = out.(Model)
	if m.detail.issue.ShortID != "bbb2" {
		t.Fatalf("B issue clobbered by stale A fetch: %+v", m.detail.issue)
	}
}

// TestDetail_StaleFetch_DroppedAcrossJump: a detail-side jump advances
// dm.gen via Model.handleJumpDetail; an in-flight fetch from before
// the jump must not seed the post-jump view. The flow is exercised at
// the Model level so the monotonic m.nextGen counter is the authority.
func TestDetail_StaleFetch_DroppedAcrossJump(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	m.view = viewDetail
	m.detail = detailFixture()
	m.detail.activeTab = tabLinks
	m.detail.tabCursor = 0
	// Bump nextGen so the active dm.gen reflects an open already done.
	m.nextGen = 5
	m.detail.gen = m.nextGen
	priorGen := m.detail.gen

	// jumpDetailMsg drives the jump path: m.handleJumpDetail allocates
	// a fresh gen, pushes prior onto navStack, and dispatches fetches.
	out, cmd := m.Update(jumpDetailMsg{ref: "11bb"})
	m = out.(Model)
	if m.detail.gen == priorGen {
		t.Fatal("gen should advance on jump")
	}
	if len(m.detail.navStack) != 1 {
		t.Fatalf("navStack length = %d, want 1", len(m.detail.navStack))
	}
	if cmd == nil {
		t.Fatal("expected jump cmd")
	}
	// Stale comments from the prior gen arrive after the jump.
	stale := commentsFetchedMsg{
		gen:      priorGen,
		comments: []CommentEntry{{ID: 99, Body: "stale"}},
	}
	out, _ = m.Update(stale)
	m = out.(Model)
	if len(m.detail.comments) != 0 {
		t.Fatalf("stale comments leaked into post-jump view: %+v", m.detail.comments)
	}
}

// TestModel_GenMonotonicAcrossJumpBackOpen: the gen-reuse regression.
// User opens A → jumps to B → backs to A → opens C from list. Without
// a Model-level monotonic counter, C's gen could equal B's gen
// (because handleBack restored A's smaller gen and the next +1 from
// that lands on B's value). A stale B fetch arriving after C is open
// would then match C.gen and corrupt the new view.
//
// This regression test seeds a stale B fetch after the open of C and
// asserts C's data survives — the gen on C must be strictly greater
// than B's gen.
func TestModel_GenMonotonicAcrossJumpBackOpen(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	// List has issues A (#1) and C (#3); B (#2) is the jump target.
	m.list.loading = false
	m.list.issues = []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "A"},
		{ProjectID: 7, UID: "01TEST-ccc3", ShortID: "ccc3", Title: "C"},
	}

	// Open A.
	out, _ := m.Update(openDetailMsg{issue: m.list.issues[0]})
	m = out.(Model)
	genA := m.detail.gen
	if genA == 0 {
		t.Fatal("genA should be non-zero after open")
	}
	// Hydrate A so the snapshot in navStack carries a non-nil issue.
	out, _ = m.Update(detailFetchedMsg{gen: genA, issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "A"}})
	m = out.(Model)

	// Jump to B (#bbb2). genB allocated from m.nextGen.
	out, _ = m.Update(jumpDetailMsg{ref: "bbb2"})
	m = out.(Model)
	genB := m.detail.gen
	if genB <= genA {
		t.Fatalf("genB (%d) should exceed genA (%d) after jump", genB, genA)
	}

	// Back to A. handleBack restores A's snapshot — including its
	// smaller gen. m.detail.gen is now genA again (this is the bug
	// surface a Model-level counter has to defend against).
	out, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = out.(Model)
	if m.detail.gen != genA {
		t.Fatalf("after back, dm.gen = %d, want %d (snapshot restore)",
			m.detail.gen, genA)
	}

	// Pop to list (Esc again from the now-top-level A). handleBack
	// returns popDetailCmd; we invoke the cmd to feed popDetailMsg
	// back into the model so the view actually transitions.
	out, popCmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = out.(Model)
	if popCmd == nil {
		t.Fatal("expected popDetailCmd from Esc on top-level detail")
	}
	out, _ = m.Update(popCmd())
	m = out.(Model)
	if m.view != viewList {
		t.Fatalf("after second Esc + popDetailMsg, view = %v, want viewList", m.view)
	}

	// Open C (#3). The new gen MUST exceed genB so a stale B fetch
	// can't pass the gen check on C.
	out, _ = m.Update(openDetailMsg{issue: m.list.issues[1]})
	m = out.(Model)
	genC := m.detail.gen
	if genC <= genB {
		t.Fatalf("genC (%d) must exceed genB (%d) — gen reuse regression",
			genC, genB)
	}

	// A stale B fetch lands. With a monotonic counter, genB != genC so
	// applyFetched drops the message. With the buggy +1-from-restored-
	// gen scheme, genB would equal genC and the stale data would seed
	// C's view.
	staleB := detailFetchedMsg{
		gen:   genB,
		issue: &Issue{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "B clobbered"},
	}
	out, _ = m.Update(staleB)
	m = out.(Model)
	if m.detail.issue == nil || m.detail.issue.ShortID != "ccc3" {
		t.Fatalf("stale B fetch leaked into C view: %+v", m.detail.issue)
	}
}

// TestDetail_MutationResp_FromListIgnored: a list-side mutation
// completing after the user opened detail must not steal the detail
// status line nor trigger a refetch in detail.
func TestDetail_MutationResp_FromListIgnored(t *testing.T) {
	api := &fakeDetailAPI{}
	km := newKeymap()
	dm := dmFixture()
	dm.gen = 3
	listDone := mutationDoneMsg{
		origin: "list",
		kind:   "close",
		resp:   &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}},
	}
	out, cmd := dm.Update(listDone, km, api)
	if cmd != nil {
		t.Fatalf("list-origin mutation must not refetch in detail, got %T", cmd)
	}
	if out.status != "" {
		t.Fatalf("detail status should be untouched, got %q", out.status)
	}
}

// TestDetail_StaleMutationResp_DroppedAcrossJump: a detail close in
// flight when the user jumps must not refetch the now-current issue's
// data with a stale generation.
func TestDetail_StaleMutationResp_DroppedAcrossJump(t *testing.T) {
	api := &fakeDetailAPI{}
	km := newKeymap()
	dm := dmFixture()
	dm.gen = 7
	stale := mutationDoneMsg{
		origin: "detail",
		gen:    1, // older than dm.gen
		kind:   "close",
		resp:   &MutationResp{Issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1"}},
	}
	out, cmd := dm.Update(stale, km, api)
	if cmd != nil {
		t.Fatalf("stale mutation must not trigger refetch, got %T", cmd)
	}
	if out.status != "" {
		t.Fatalf("stale mutation must not seed status, got %q", out.status)
	}
}

// TestList_DetailMutation_Ignored: a detail-origin mutation lands on
// the list view after the user pops; the list must not seed its status
// line or refetch from it.
func TestList_DetailMutation_Ignored(t *testing.T) {
	api := &fakeListAPI{}
	km := newKeymap()
	sc := scope{projectID: 7}
	lm := listModel{actor: "tester"}
	detailDone := mutationDoneMsg{
		origin: "detail",
		gen:    1,
		kind:   "close",
		resp:   &MutationResp{Issue: &Issue{UID: "01TEST-99zz", ShortID: "99zz"}},
	}
	out, cmd := lm.Update(detailDone, km, api, sc)
	if cmd != nil {
		t.Fatalf("detail-origin mutation must not refetch list, got %T", cmd)
	}
	if out.status != "" {
		t.Fatalf("list status should be untouched, got %q", out.status)
	}
}

// TestDetail_Open_SeedsActorFromList: opening detail through the
// model-level handler seeds dm.actor from lm.actor so a detail
// mutation dispatched through Model.Update reaches the daemon with
// the resolved identity instead of the empty string.
func TestDetail_Open_SeedsActorFromList(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "wes")
	m := newDetailHostModel(Options{}, 7)
	iss := Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if m.detail.actor != "wes" {
		t.Fatalf("dm.actor = %q, want wes (seeded from list.actor)", m.detail.actor)
	}
}

// TestDetail_Mutation_ThroughModelCarriesActor: a close dispatched via
// Model.Update reaches the daemon with the seeded actor. Regression
// for the bug where dm.actor was never populated and the daemon
// rejected with empty actor.
func TestDetail_Mutation_ThroughModelCarriesActor(t *testing.T) {
	t.Setenv("KATA_AUTHOR", "wes")
	m := initialModel(Options{})
	m.scope = scope{projectID: 7}
	api := &fakeDetailAPI{
		mutationResult: &MutationResp{Issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Status: "closed"}},
	}
	iss := Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}
	// We can't pass api through Model.Update directly; thread it via
	// the detail sub-model so handleOpenDetail's actor seeding lands
	// before the close dispatch.
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if m.detail.actor == "" {
		t.Fatal("actor not seeded by open")
	}
	dm, cmd := m.detail.Update(runeKey('x'), m.keymap, api)
	if cmd == nil {
		t.Fatal("expected close cmd")
	}
	_ = dm
	msg := cmd()
	done, ok := msg.(mutationDoneMsg)
	if !ok {
		t.Fatalf("expected mutationDoneMsg, got %T", msg)
	}
	if api.lastActor != "wes" {
		t.Fatalf("api.lastActor = %q, want wes", api.lastActor)
	}
	if done.origin != "detail" {
		t.Fatalf("mutationDoneMsg.origin = %q, want detail", done.origin)
	}
}

// TestDetail_Jump_PreservesActor: a jump preserves dm.actor so a
// mutation in the post-jump view still carries the resolved identity.
func TestDetail_Jump_PreservesActor(t *testing.T) {
	api := &fakeDetailAPI{
		getIssueResult: &Issue{UID: "01TEST-11bb", ShortID: "11bb", Title: "linked"},
	}
	dm := detailFixture()
	dm.actor = "wes"
	dm.activeTab = tabLinks
	dm.tabCursor = 0
	km := newKeymap()
	dm, cmd := dm.Update(tea.KeyMsg{Type: tea.KeyEnter}, km, api)
	if cmd == nil {
		t.Fatal("expected jump cmd")
	}
	if dm.actor != "wes" {
		t.Fatalf("post-jump dm.actor = %q, want wes (preserved)", dm.actor)
	}
}

// TestDetail_Open_AdvancesGenAcrossReopens: opening the same issue
// twice advances the generation each time so any in-flight fetch from
// the first open is dropped on the second.
func TestDetail_Open_AdvancesGenAcrossReopens(t *testing.T) {
	m := newDetailHostModel(Options{}, 7)
	iss := Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	first := m.detail.gen
	out, _ = m.Update(popDetailMsg{})
	m = out.(Model)
	out, _ = m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	if m.detail.gen <= first {
		t.Fatalf("second open did not advance gen: first=%d second=%d",
			first, m.detail.gen)
	}
}
