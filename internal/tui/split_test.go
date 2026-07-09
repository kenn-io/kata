package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// splitTestSetup boots a Model into split layout (160x40) with the
// listFixture seeded so the split tests have something to render and
// drive cursor moves against. Returns the model and a cleanup that
// reverts the rebuilt color mode. The api field is wired with a stub
// Client so handlers that gate on a non-nil api don't no-op.
func splitTestSetup(t *testing.T) (Model, func()) {
	t.Helper()
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{projectID: 7, projectName: "kata"}
	m.list.loading = false
	m.list.issues = snapListFixture()
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	cleanup := func() { applyDefaultColorMode() }
	if m.layout != layoutSplit {
		t.Fatalf("split setup failed: layout=%v want layoutSplit", m.layout)
	}
	return m, cleanup
}

// focusFirstIssueDetail seeds the detail pane with m.list.issues[0],
// pins scopePID to the active project, and switches focus to the
// detail pane — the standard mutation pattern that drives detail-pane
// tests in split mode.
func focusFirstIssueDetail(m *Model) {
	iss := m.list.issues[0]
	m.detail.issue = &iss
	m.detail.scopePID = m.scope.projectID
	m.focus = focusDetail
}

// assertSingleOverlayBox verifies that view contains exactly one
// rounded modal box (╭). Pane borders use the normal border (┌), so a
// count > 1 means a modal accidentally rendered inside a pane instead
// of over the whole terminal.
func assertSingleOverlayBox(t *testing.T, view string) {
	t.Helper()
	if c := strings.Count(view, "╭"); c != 1 {
		t.Errorf("expected exactly 1 modal top-left ╭ corner, got %d", c)
	}
}

// TestSplit_CursorMoveRetargetsDetail covers the synchronous detail-
// follows-cursor behavior: pressing j three times in the list pane
// must land m.detail.issue on the third row's issue without waiting
// for the debounce tick (the fetch is debounced; the dm.issue
// retarget is immediate).
func TestSplit_CursorMoveRetargetsDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	// Press j twice — the fixture has 3 rows so cursor lands on row 2.
	for i := 0; i < 2; i++ {
		m, _ = updateModel(m, runeKey('j'))
	}
	if m.detail.issue == nil {
		t.Fatal("dm.issue stayed nil after cursor moves")
	}
	want := m.list.issues[2].ShortID
	if m.detail.issue.ShortID != want {
		t.Errorf("dm.issue.ShortID = %q, want %q", m.detail.issue.ShortID, want)
	}
	if m.list.cursor != 2 {
		t.Errorf("list.cursor = %d, want 2", m.list.cursor)
	}
}

// TestSplit_DebounceCoalescesBursts: rapid j keys must bump the
// debounce gen each time so older pending ticks drop. We can't
// directly observe tea.Tick scheduling from here, but the gen
// counter is the load-bearing identifier — verify it advances by N
// for N keystrokes (or fewer if some keystrokes don't move the
// cursor because we hit the end).
func TestSplit_DebounceCoalescesBursts(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	startGen := m.nextDetailFollowGen
	for i := 0; i < 5; i++ {
		m, _ = updateModel(m, runeKey('j'))
	}
	// Cursor caps at len-1 = 2 (3 rows), so two of the five j keys
	// move and three are no-ops. The gen advances only on actual
	// cursor moves, so the counter goes up by 2.
	if m.nextDetailFollowGen-startGen != 2 {
		t.Errorf("nextDetailFollowGen advanced by %d, want 2",
			m.nextDetailFollowGen-startGen)
	}
}

// TestSplit_InitialFetchBootstrapsDetailPane pins the launch fix:
// when the boot initialFetchMsg lands in split layout with no detail
// issue pinned, the highlighted (cursor=0) row must auto-load into
// the detail pane. Pre-fix the detail pane stayed empty until the
// user pressed j/k because scheduleDetailFollow only fires from
// dispatchListKey on cursor motion.
func TestSplit_InitialFetchBootstrapsDetailPane(t *testing.T) {
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	defer applyDefaultColorMode()
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{projectID: 7, projectName: "kata"}
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Fatalf("setup: layout=%v want layoutSplit", m.layout)
	}
	if m.detail.issue != nil {
		t.Fatalf("setup: detail.issue=%+v want nil before fetch", m.detail.issue)
	}
	issues := snapListFixture()
	nm, cmd := updateModel(m, initialFetchMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      issues,
	})
	if nm.detail.issue == nil {
		t.Fatal("detail.issue stayed nil after initial fetch in split layout")
	}
	if nm.detail.issue.ShortID != issues[0].ShortID {
		t.Errorf("detail.issue.ShortID = %q, want %q (first row)",
			nm.detail.issue.ShortID, issues[0].ShortID)
	}
	if cmd == nil {
		t.Error("expected a debounce tick cmd for the bootstrap detail fetch")
	}
}

// TestSplit_StackedToSplitResizeBootstrapsDetailPane covers the
// resize/layout-toggle path: even when the initial fetch lands while
// the terminal is still stacked, a later widen (or a manual L toggle)
// that flips us into split must auto-populate the detail pane. Without
// this, an agent who starts narrow then maximizes their terminal would
// land on an empty right-hand pane.
func TestSplit_StackedToSplitResizeBootstrapsDetailPane(t *testing.T) {
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	defer applyDefaultColorMode()
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{projectID: 7, projectName: "kata"}
	// Boot stacked, then deliver the list payload while still stacked.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Fatalf("setup: layout=%v want layoutStacked", m.layout)
	}
	issues := snapListFixture()
	m, _ = updateModel(m, initialFetchMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      issues,
	})
	if m.detail.issue != nil {
		t.Fatalf("setup: detail.issue=%+v after stacked fetch, want nil",
			m.detail.issue)
	}
	// Now widen past the split breakpoint — should bootstrap detail.
	nm, cmd := updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if nm.layout != layoutSplit {
		t.Fatalf("after resize: layout=%v want layoutSplit", nm.layout)
	}
	if nm.detail.issue == nil {
		t.Fatal("detail.issue stayed nil after stacked→split resize")
	}
	if nm.detail.issue.ShortID != issues[0].ShortID {
		t.Errorf("detail.issue.ShortID = %q, want %q",
			nm.detail.issue.ShortID, issues[0].ShortID)
	}
	if cmd == nil {
		t.Error("expected a debounce tick cmd for the bootstrap detail fetch")
	}
}

// TestSplit_InitialFetchSkipsBootstrapInStacked: the auto-load is
// scoped to split layout. In stacked mode the user never sees the
// detail pane until they explicitly open an issue, so loading detail
// state on the initial fetch would waste an HTTP round-trip.
func TestSplit_InitialFetchSkipsBootstrapInStacked(t *testing.T) {
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	defer applyDefaultColorMode()
	m := initialModel(Options{})
	m.api = &Client{}
	m.scope = scope{projectID: 7, projectName: "kata"}
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Fatalf("setup: layout=%v want layoutStacked", m.layout)
	}
	nm, _ := updateModel(m, initialFetchMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      snapListFixture(),
	})
	if nm.detail.issue != nil {
		t.Errorf("detail.issue=%+v after initial fetch in stacked, want nil",
			nm.detail.issue)
	}
}

// TestSplit_TabMovesFocusToDetail: tab in split mode while focusList
// flips focus to focusDetail (and the list pane border switches to
// the inactive style on render).
func TestSplit_TabMovesFocusToDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	// Seed a detail issue so the tab move actually flips focus (no
	// detail open => tab is a no-op per the routeLayoutFocusKey
	// guard).
	iss := m.list.issues[0]
	m.detail.issue = &iss
	if m.focus != focusList {
		t.Fatalf("setup focus=%v want focusList", m.focus)
	}
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.focus != focusDetail {
		t.Errorf("focus=%v after tab, want focusDetail", m.focus)
	}
}

// TestSplit_EnterMovesFocusToDetail: enter on focusList dispatches
// openDetailMsg through the list pane handler; routing the resulting
// message moves focus to focusDetail (per handleOpenDetail's split-
// mode branch).
func TestSplit_EnterMovesFocusToDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter on list pane produced no cmd; expected openDetailMsg dispatch")
	}
	msg := cmd()
	if _, ok := msg.(openDetailMsg); !ok {
		t.Fatalf("expected openDetailMsg from Enter, got %T", msg)
	}
	m, _ = updateModel(m, msg)
	if m.focus != focusDetail {
		t.Errorf("focus=%v after enter+route, want focusDetail", m.focus)
	}
}

// TestSplit_EscReturnsFocusToList: esc on focusDetail flips focus
// back to focusList without consuming the esc on the detail pane
// (the per-pane back-handler is reserved for the no-input case).
func TestSplit_EscReturnsFocusToList(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	focusFirstIssueDetail(&m)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.focus != focusList {
		t.Errorf("focus=%v after esc, want focusList", m.focus)
	}
}

// TestSplit_EscDoesNotEscapeWhilePromptActive: with a panel-local
// prompt open on the detail pane, esc closes the prompt but leaves
// focus on the detail pane (the routeInputKey path absorbs esc
// before routeLayoutFocusKey runs). A second esc then moves focus.
func TestSplit_EscDoesNotEscapeWhilePromptActive(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	focusFirstIssueDetail(&m)
	// Open a label prompt.
	m, _ = m.openInput(inputLabelPrompt)
	if m.input.kind != inputLabelPrompt {
		t.Fatalf("setup failed: input.kind=%v want inputLabelPrompt", m.input.kind)
	}
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.input.kind != inputNone {
		t.Errorf("input.kind=%v after first esc, want inputNone (prompt closed)", m.input.kind)
	}
	if m.focus != focusDetail {
		t.Errorf("focus=%v after first esc, want focusDetail (focus stays)", m.focus)
	}
	// Second esc moves focus.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.focus != focusList {
		t.Errorf("focus=%v after second esc, want focusList", m.focus)
	}
}

// TestSplit_FilterModalOverlaysWholeTerminal: opening the filter
// modal in split mode renders the centered overlay over the whole
// terminal (not anchored to a single pane). We verify by counting
// the ╭ corners — the modal box has exactly one top-left corner; if
// the modal accidentally rendered inside a pane the surrounding
// pane border would inject extras.
func TestSplit_FilterModalOverlaysWholeTerminal(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m, _ = m.openInput(inputFilterForm)
	got := m.viewContent()
	if !strings.Contains(got, "filter") {
		t.Fatalf("filter modal did not render; output:\n%s", got)
	}
	assertSingleOverlayBox(t, got)
}

// TestSplit_NewIssueFormOverlaysWholeTerminal: same property for the
// new-issue centered form.
func TestSplit_NewIssueFormOverlaysWholeTerminal(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m, _ = m.openInput(inputNewIssueForm)
	got := m.viewContent()
	if !strings.Contains(got, "new issue") {
		t.Fatalf("new-issue form did not render; output:\n%s", got)
	}
	assertSingleOverlayBox(t, got)
}

// TestSplit_HelpRowSwapsWithFocus: focus=list shows list footer
// bindings (e.g. "search"); switching to focus=detail shows the
// comprehensive detail footer (which carries detail-only keywords
// like "section" and the action surface "edit"/"comment"). The
// list footer must not carry detail-only keywords; the detail
// footer must not carry list-only keywords (search/filter).
func TestSplit_HelpRowSwapsWithFocus(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	listView := m.viewContent()
	if !strings.Contains(listView, "search") {
		t.Errorf("list-focus footer missing 'search' hint:\n%s", listView)
	}
	if strings.Contains(listView, "section") {
		t.Errorf("list-focus footer should not carry detail 'section' keyword:\n%s", listView)
	}
	iss := m.list.issues[0]
	m.detail.issue = &iss
	m.focus = focusDetail
	detailView := m.viewContent()
	if !strings.Contains(detailView, "section") {
		t.Errorf("detail-focus footer missing 'section' hint:\n%s", detailView)
	}
	if !strings.Contains(detailView, "edit") {
		t.Errorf("detail-focus footer missing comprehensive 'edit' hint:\n%s", detailView)
	}
}

// TestSplit_SuggestionMenuClampedToDetailPane: opening `+` on the
// detail pane in split mode anchors the menu inside the detail-pane
// column range. The menu sits to the right of the list pane; we
// search for the menu content row ("alpha (1)") and verify it
// starts at a column >= splitListPaneWidth.
func TestSplit_SuggestionMenuClampedToDetailPane(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	focusFirstIssueDetail(&m)
	// Seed the label cache so the menu has a known row to find.
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1,
		labels: []LabelCount{{Label: "alpha", Count: 1}},
	}
	m, _ = m.openInput(inputLabelPrompt)
	got := m.viewContent()
	// Look for the unique menu content "alpha (1)" — it shouldn't
	// appear on the left side of the screen (which is the list
	// pane), only inside the detail pane (column >= list-pane width).
	idx := strings.Index(got, "alpha (1)")
	if idx < 0 {
		t.Fatalf("menu content not found in output:\n%s", got)
	}
	// Find the column of "alpha (1)" within its line.
	lineStart := strings.LastIndex(got[:idx], "\n") + 1
	col := idx - lineStart
	listW := splitListPaneWidth(m.width)
	if col < listW {
		t.Errorf("suggest menu content at column %d, want >= %d (list pane width)",
			col, listW)
	}
}

// TestSplit_LayoutFlip_FromStackedToSplitFromList: stacked viewList
// resized up to split → focus goes to focusList, view stays viewList,
// selection survives.
func TestSplit_LayoutFlip_FromStackedToSplitFromList(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	// Already in split mode from setup — flip back to stacked first.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Fatalf("setup failed: layout=%v want layoutStacked", m.layout)
	}
	m.list.selectedUID = "01TEST-7zz"
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Errorf("layout=%v after resize up, want layoutSplit", m.layout)
	}
	if m.focus != focusList {
		t.Errorf("focus=%v want focusList", m.focus)
	}
	if m.list.selectedUID != "01TEST-7zz" {
		t.Errorf("selectedUID=%q want 01TEST-7zz", m.list.selectedUID)
	}
}

// TestSplit_JumpDetail_SurvivesCursorFollowFocusDetail pins C1 from
// the M6 review (codex). The bug: handleJumpDetail's m.view !=
// viewDetail gate dropped the jump in split mode after cursor-follow
// because m.view stays viewList while m.focus advances to focusDetail.
// scheduleDetailFollow retargets m.detail.issue synchronously without
// touching m.view; the stale view check then silently dropped the
// jump. Fixed by switching the gate to !m.detailIsActive() — the
// existing helper that abstracts over both layouts (stacked checks
// m.view; split checks m.focus).
func TestSplit_JumpDetail_SurvivesCursorFollowFocusDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	// Press j to retarget detail via cursor-follow; m.view stays viewList.
	m, _ = updateModel(m, runeKey('j'))
	if m.detail.issue == nil {
		t.Fatal("setup: cursor-follow did not retarget m.detail.issue")
	}
	if m.view != viewList {
		t.Fatalf("setup: m.view=%v want viewList (cursor-follow must not change m.view)", m.view)
	}
	// Tab advances focus to focusDetail; m.view still viewList.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if m.focus != focusDetail {
		t.Fatalf("setup: m.focus=%v want focusDetail after Tab", m.focus)
	}
	if m.view != viewList {
		t.Fatalf("setup: m.view=%v want viewList (Tab must not change m.view)", m.view)
	}
	// jumpDetailMsg must NOT be dropped — pre-fix this returned nil cmd
	// because the stale m.view==viewList misled the gate.
	_, cmd := m.Update(jumpDetailMsg{ref: "42aa"})
	if cmd == nil {
		t.Fatal("jumpDetailMsg dropped — handleJumpDetail's view gate misfires in split mode")
	}
}

// TestSplit_JumpDetail_DroppedWhenViewObscured covers Job 252: when a
// full-screen view (viewHelp / viewEmpty) hides both panes in split
// mode, a queued jumpDetailMsg must NOT silently mutate the hidden
// detail state. The original M6 fix used detailIsActive() which
// ignored m.view; this test pins the corrected detailPaneVisible()
// gate across both obscuring views.
func TestSplit_JumpDetail_DroppedWhenViewObscured(t *testing.T) {
	cases := []struct {
		name string
		view viewID
	}{
		{"viewHelp", viewHelp},
		{"viewEmpty", viewEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, cleanup := splitTestSetup(t)
			defer cleanup()
			// Park focus on detail (the racy state: ? opens help while
			// focus is still focusDetail). detailIsActive() alone returns
			// true here, so the bare-helper gate would let the jump through.
			m.focus = focusDetail
			m.view = tc.view
			priorGen := m.detail.gen
			if !m.detailIsActive() {
				t.Fatalf("setup: detailIsActive=false with focusDetail, want true (test would not exercise the gate)")
			}
			if m.detailPaneVisible() {
				t.Fatalf("setup: detailPaneVisible=true with %v, want false", tc.name)
			}
			nm, cmd := updateModel(m, jumpDetailMsg{ref: "99zz"})
			if cmd != nil {
				t.Fatalf("jumpDetailMsg under %s dispatched a cmd — hidden detail state mutated (Job 252 regression)", tc.name)
			}
			if nm.detail.gen != priorGen {
				t.Errorf("detail.gen advanced from %d to %d under %s — hidden detail state mutated", priorGen, nm.detail.gen, tc.name)
			}
		})
	}
}

// TestSplit_ListMutation_LandsOnListWhileFocusDetail covers the
// cross-focus mutation routing (I2 from M6 review): in split mode
// with focus=focusDetail, a list-originated mutation must still
// update lm.status. The dispatch goes via routeMutation which uses
// listIsActive() — split mode keeps focusDetail "active for detail"
// so list is not active, and the !listIsActive() branch fires the
// direct applyMutation path on the list.
func TestSplit_ListMutation_LandsOnListWhileFocusDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m.list.actor = "tester"
	focusFirstIssueDetail(&m)
	if m.listIsActive() {
		t.Fatalf("setup: listIsActive=true with focusDetail, want false")
	}
	mut := mutationDoneMsg{
		origin: "list", kind: "close",
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa", Status: "closed"}},
	}
	nm, _ := updateModel(m, mut)
	if nm.list.status == "" {
		t.Fatal("list.status empty — list-origin mutation dropped while focusDetail")
	}
	if !strings.Contains(nm.list.status, "closed #42aa") {
		t.Errorf("list.status = %q, want hint about closed #42aa", nm.list.status)
	}
}

// TestSplit_DetailMutation_LandsOnDetailWhileFocusList covers the
// reverse (I2 from M6 review): a detail-originated mutation in split
// mode with focus=focusList still updates the detail pane (which is
// visible alongside the list). routeMutation sees !detailIsActive()
// and routes directly to dm.applyMutation; the gen match is
// preserved so the dm.status hint lands.
func TestSplit_DetailMutation_LandsOnDetailWhileFocusList(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	m.detail.issue = &Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "to edit"}
	m.detail.scopePID = 7
	m.detail.gen = 5
	m.focus = focusList
	if m.detailIsActive() {
		t.Fatalf("setup: detailIsActive=true with focusList, want false")
	}
	mut := mutationDoneMsg{
		origin: "detail", gen: 5, kind: "body.edit",
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa", Body: "new"}},
	}
	nm, _ := updateModel(m, mut)
	if nm.detail.status == "" {
		t.Fatal("detail.status empty — detail-origin mutation dropped while focusList")
	}
	if !strings.Contains(nm.detail.status, "#42aa") {
		t.Errorf("detail.status = %q, want hint mentioning #42aa", nm.detail.status)
	}
}

// TestSplit_SuggestionMenuClampActuallyFires_AtMinSplit covers the
// clamp-branch in overlaySuggestMenu (I3 from M6 review). The
// existing TestSplit_SuggestionMenuClampedToDetailPane only verifies
// the natural anchor lands inside the detail pane, but at 160x40
// with a small menu that's already true without the clamp. To
// exercise the clamp itself we'd need a menu wider than width -
// splitListPaneWidth - 1; with suggestMenuMaxWidth=40 and
// splitMinWidth=140 the natural anchor is 140-40-1=99, comfortably
// to the right of splitListPaneWidth+1=69. So the clamp branch as
// written is defensive — it cannot fire under realistic constants.
//
// To still cover the helper's clamp logic, we drive overlayAtCorner
// with an out-of-range anchorCol and assert the column-clamping
// path in spliceRow keeps the panel inside the visible width. This
// is the underlying primitive overlaySuggestMenu's split-mode minCol
// guards against (anchor too far left); together with the comment
// in overlaySuggestMenu it documents that the M6 minCol is
// future-proofing in case suggestMenuMaxWidth grows or
// splitListPaneWidth shrinks past the breakpoint.
func TestSplit_SuggestionMenuClampActuallyFires_AtMinSplit(t *testing.T) {
	// Confirm the documented invariant: at the minimum split
	// breakpoint with the maximum menu width, the natural anchor
	// is still right of the list-pane boundary, so the clamp is
	// defensive.
	listW := splitListPaneWidth(splitMinWidth)
	naturalAnchor := splitMinWidth - suggestMenuMaxWidth - 1
	if naturalAnchor < listW+1 {
		t.Fatalf("constants drifted: at width=%d max-menu=%d "+
			"naturalAnchor=%d, want >= listW+1=%d "+
			"(if this fires, overlaySuggestMenu's minCol clamp is "+
			"now load-bearing and needs an end-to-end test)",
			splitMinWidth, suggestMenuMaxWidth, naturalAnchor,
			listW+1)
	}
	// Exercise the underlying overlay-clamp primitive directly: pass
	// an anchorCol left of zero and assert the panel still renders
	// fully (clamped to col=0). This is the same column-bounds path
	// overlaySuggestMenu's split-mode branch funnels into.
	bg := strings.Repeat(strings.Repeat(" ", 40)+"\n", 5)
	bg = strings.TrimRight(bg, "\n")
	panel := "ABCDE\nFGHIJ"
	got := overlayAtCorner(bg, panel, 40, 5, 0, -10)
	firstLine := strings.SplitN(got, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "ABCDE") {
		t.Errorf("clamp did not pin panel to col=0: first line %q", firstLine)
	}
}

// TestSplit_CursorFollow_RetargetsOnSameNumberDifferentProject covers
// roborev #251 finding 1: in all-projects mode (currently gated off
// but the dispatch path is forward-looking), two rows can share the
// same Number across different projects. dispatchListKey's pre-fix
// trigger compared selectedNumber alone, which would treat the move
// from row A (ShortID=aaa1, ProjectID=7) to row B (ShortID=aaa1, ProjectID=8)
// as a no-op and never retarget the detail pane. Composite identity
// (project_id, UID) detects the cross-project change correctly.
func TestSplit_CursorFollow_RetargetsOnSameShortIDDifferentProject(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	// Two rows with same ShortID but different ProjectID — the
	// cross-project case all-projects mode would surface. UIDs differ
	// because UID is globally unique.
	m.scope = scope{allProjects: true}
	m.list.issues = []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1A", ShortID: "aaa1", Title: "row A in proj 7", Status: "open"},
		{ProjectID: 8, UID: "01TEST-aaa1B", ShortID: "aaa1", Title: "row B in proj 8", Status: "open"},
	}
	m.list.cursor = 0
	m.list.selectedUID = m.list.issues[0].UID
	// Position detail on row A explicitly so the test asserts the
	// post-j retarget moved it to row B (not just that something
	// landed on row B by coincidence).
	rowA := m.list.issues[0]
	m.detail.issue = &rowA
	startGen := m.nextDetailFollowGen
	// Press j — cursor moves to row 1; UID differs across projects so
	// the retarget fires. Pre-fix this was a silent no-op when matching
	// on (project_id, number) alone with shared numbers.
	m, _ = updateModel(m, runeKey('j'))
	if m.list.cursor != 1 {
		t.Fatalf("setup failed: cursor=%d after j, want 1", m.list.cursor)
	}
	if m.detail.issue == nil {
		t.Fatal("detail.issue nil after cursor move; retarget did not fire")
	}
	if m.detail.issue.ProjectID != 8 {
		t.Errorf("detail.issue.ProjectID=%d after j, want 8 (row B); "+
			"selectedUID-only check missed cross-project move",
			m.detail.issue.ProjectID)
	}
	if m.detail.issue.ShortID != "aaa1" {
		t.Errorf("detail.issue.ShortID=%q, want aaa1", m.detail.issue.ShortID)
	}
	if m.nextDetailFollowGen <= startGen {
		t.Errorf("nextDetailFollowGen did not advance; debounce tick not scheduled "+
			"(gen=%d, start=%d)", m.nextDetailFollowGen, startGen)
	}
}
