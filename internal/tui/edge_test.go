package tui

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestEdge_WindowResize_NoPanic feeds a wide-then-narrow WindowSizeMsg
// pair through Model.Update and verifies the list still renders without
// panic at the smaller width. We exercise the row-truncation path at
// 80 cells — the M5 narrow-hint threshold — where the title column
// flexes to its 20-cell floor. Below 80, View short-circuits to the
// narrow hint (covered by narrow_terminal_test.go). The fixture is
// the same three-row mix used by snapshot tests so the comparison
// stays deterministic.
func TestEdge_WindowResize_NoPanic(t *testing.T) {
	t.Setenv("KATA_COLOR_MODE", "none")
	applyDefaultColorMode(io.Discard)
	prior := renderNow
	renderNow = func() time.Time { return snapshotFixedNow }
	defer func() { renderNow = prior; applyDefaultColorMode(io.Discard) }()

	m := initialModel(Options{})
	m.list.loading = false
	m.list.issues = snapListFixture()
	m.list.issues[0].Title = "fix login bug on Safari with an unusually long regression title"

	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 140, Height: 30})
	wide := m.View()
	if !strings.Contains(wide, "fix login bug on Safari with an unusually long regression title") {
		t.Fatalf("wide render missing full title:\n%s", wide)
	}

	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 80, Height: 30})
	if m.width != 80 || m.height != 30 {
		t.Fatalf("resize not applied: width=%d height=%d", m.width, m.height)
	}
	narrow := m.View()
	// At 80 cols the title column flexes to 20 (floor: fixed cols sum
	// to 42, leaving 38 — less than the wide render's title width).
	// The cursor marker must still be present.
	if !strings.Contains(narrow, "▶") {
		t.Fatalf("narrow render missing cursor marker:\n%s", narrow)
	}
	// The full title would not fit at width=80 — the truncate helper
	// either replaces the tail with "…" or leaves the row narrower than
	// the wide render's row. We assert the ellipsis to lock in the
	// truncation behavior — if the renderer ever stops truncating, this
	// is a useful regression catch.
	if !strings.Contains(narrow, "…") {
		t.Fatalf("narrow render did not truncate any title (no ellipsis):\n%s", narrow)
	}
}

// TestEdge_SSEDuringSearchPrompt: the M3a inline command bar is open;
// an SSE eventReceivedMsg arrives mid-typing. The next keystroke must
// reach the bar (canQuit gates the global keys via m.input.kind), and
// the SSE-driven pendingRefetch can flip in the background without
// churning the bar. After Enter commits, the bar closes and the
// (already live-applied) filter remains.
//
// Search is client-side so the bar's commit does NOT dispatch a
// refetch — the cursor clamp + filter mirror handle the visual update.
// Status filter changes still dispatch a refetch (covered separately).
func TestEdge_SSEDuringSearchPrompt(t *testing.T) {
	m := newTestModel()
	m.list.issues = []Issue{{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}}

	// Open the search bar with '/'.
	m = openBarFromCmd(t, m, '/')
	if m.input.kind != inputSearchBar {
		t.Fatalf("bar did not open on '/', got kind=%v", m.input.kind)
	}

	// Type 'a' before the SSE event arrives. Live mirrors into filter.
	m, cmd := updateModel(m, runeKey('a'))
	if cmd != nil {
		t.Fatalf("typing 'a' must not return a cmd, got %T", cmd)
	}
	if v := m.input.activeField().value(); v != "a" {
		t.Fatalf("bar value = %q, want %q", v, "a")
	}

	// SSE event lands while the bar is still open. pendingRefetch
	// flips and a debounce tick is queued; the bar state is untouched.
	m, sseCmd := updateModel(m, eventReceivedMsg{projectID: 7})
	if m.input.kind != inputSearchBar {
		t.Fatal("SSE event closed the bar; should be transparent to it")
	}
	if v := m.input.activeField().value(); v != "a" {
		t.Fatalf("SSE event mutated bar value: %q, want %q", v, "a")
	}
	if !m.pendingRefetch {
		t.Fatal("pendingRefetch must be set by SSE event regardless of input")
	}
	if sseCmd == nil {
		t.Fatal("SSE event must return a cmd (debounce tick)")
	}

	// Continue typing 'b' — bar accepts the rune.
	m, cmd = updateModel(m, runeKey('b'))
	if cmd != nil {
		t.Fatalf("typing 'b' after SSE must not return a cmd, got %T", cmd)
	}
	if v := m.input.activeField().value(); v != "ab" {
		t.Fatalf("bar value = %q after second keystroke, want %q", v, "ab")
	}

	// Enter commits — bar closes, filter stays. No refetch (Search is
	// client-side; the bar already mirrored the value live).
	m, _ = updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.kind != inputNone {
		t.Fatal("Enter did not close the bar")
	}
	if m.list.filter.Search != "ab" {
		t.Fatalf("filter.Search = %q, want %q", m.list.filter.Search, "ab")
	}
}

// TestEdge_IdentitySelection_FollowsIssueAcrossReorder: a refetch
// reorders rows (issues come back sorted by updated_at DESC, so any
// background mutation can shuffle them). The cursor must stay on the
// same issue rather than the same index.
func TestEdge_IdentitySelection_FollowsIssueAcrossReorder(t *testing.T) {
	m := newTestModel()
	m.list.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha"},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "beta"},
		{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "gamma"},
	}
	m.list.cursor = 1
	m.list.selectedUID = "01TEST-bbb2" // cursor is on bbb2 ("beta")

	// Simulate an SSE-driven refetch that reorders: bbb2 moved to row 0
	// because it was just updated. With positional selection the cursor
	// would still point at index 1 (now "alpha"), silently changing
	// what the user sees as selected.
	nm, _ := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues: []Issue{
			{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "beta"},
			{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha"},
			{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "gamma"},
		},
	})
	assertSelection(t, nm, 0, "01TEST-bbb2")
}

// TestEdge_IdentitySelection_FallsBackWhenIssueDisappears: when the
// previously-selected issue is no longer in the refetched list (e.g.
// soft-deleted, or filter narrowed it out), the cursor falls back to
// the same index clamped to the new visible range and re-records the
// issue under it.
func TestEdge_IdentitySelection_FallsBackWhenIssueDisappears(t *testing.T) {
	m := newTestModel()
	m.list.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha"},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "beta"},
		{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "gamma"},
	}
	m.list.cursor = 1
	m.list.selectedUID = "01TEST-bbb2"

	nm, _ := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues: []Issue{
			{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha"},
			// bbb2 disappeared.
			{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "gamma"},
		},
	})
	assertSelection(t, nm, 1, "01TEST-ccc3")
}

// TestEdge_PageUpPageDown_MovesCursorInChunks: pgup/pgdown shift the
// cursor by pageStep rows so navigating long lists doesn't require
// hundreds of j/k presses.
func TestEdge_PageUpPageDown_MovesCursorInChunks(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	m.list.issues = makeTestIssues(50)
	m.list.cursor = 5

	// pgdown advances by pageStep (10).
	nm, _ := updateModel(m, tea.KeyMsg{Type: tea.KeyPgDown})
	assertSelection(t, nm, 15, "01TEST-r016")

	// pgup walks back by pageStep.
	nm, _ = updateModel(nm, tea.KeyMsg{Type: tea.KeyPgUp})
	if nm.list.cursor != 5 {
		t.Fatalf("after pgup, cursor = %d, want 5", nm.list.cursor)
	}
}

// TestEdge_PageDown_ClampsAtEnd: pgdown near the end clamps to the
// last row rather than walking past the slice.
func TestEdge_PageDown_ClampsAtEnd(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	m.list.issues = makeTestIssues(12)
	m.list.cursor = 8

	nm, _ := updateModel(m, tea.KeyMsg{Type: tea.KeyPgDown})
	if nm.list.cursor != 11 {
		t.Fatalf("cursor = %d, want 11 (clamped to last row)", nm.list.cursor)
	}
}

// TestEdge_ListViewport_KeepsCursorVisible: a list of 100 rows with a
// height budget of 10 must render only the cursor's neighborhood, not
// every row. We verify the rendered output contains the cursor row's
// title and excludes rows far from the cursor.
func TestEdge_ListViewport_KeepsCursorVisible(t *testing.T) {
	lm := newListModel()
	lm.loading = false
	issues := make([]Issue, 100)
	for i := range issues {
		sid := fmt.Sprintf("v%03d", i+1)
		issues[i] = Issue{
			UID:     "01TEST-" + sid,
			ShortID: sid,
			Title:   rowTitleFor(i + 1),
			Status:  "open",
		}
	}
	lm.issues = issues
	lm.cursor = 50

	out := lm.View(120, 30, viewChrome{}) // height=30 leaves enough room for chrome + ~14 data rows
	if !strings.Contains(out, rowTitleFor(51)) {
		t.Fatalf("cursor row missing from viewport:\n%s", out)
	}
	// A row 30+ away from the cursor must NOT be in the rendered output.
	if strings.Contains(out, rowTitleFor(1)) {
		t.Fatalf("first row leaked into a windowed render of 100 issues:\n%s", out)
	}
	if strings.Contains(out, rowTitleFor(100)) {
		t.Fatalf("last row leaked into windowed render with cursor in middle:\n%s", out)
	}
}

// rowTitleFor produces a unique, identifiable title for row n so the
// viewport test can grep for specific rows in the rendered output.
func rowTitleFor(n int) string {
	return "row-id-" + numToTag(n)
}

// numToTag formats n for use inside a test title without depending on
// fmt.Sprintf (keeps the helper's intent obvious in the test harness).
func numToTag(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestEdge_RefetchAfterRenderFilterChangeKeepsWorkingSet: render filter
// changes no longer create cache slots. A working-set fetch dispatched
// before a status/search/owner/labels change should still populate the
// full list; filteredIssues narrows what renders afterward.
func TestEdge_RefetchAfterRenderFilterChangeKeepsWorkingSet(t *testing.T) {
	m := newTestModel()
	m.list.issues = []Issue{{UID: "01TEST-99zz", ShortID: "99zz", Title: "current-filter row"}}
	m.list.filter = ListFilter{Status: "open"}

	fetched := refetchedMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues: []Issue{
			{UID: "01TEST-aaa1", ShortID: "aaa1", Status: "closed", Title: "closed row"},
			{UID: "01TEST-bbb2", ShortID: "bbb2", Status: "open", Title: "open row"},
		},
	}
	nm, _ := updateModel(m, fetched)
	if len(nm.list.issues) != 2 {
		t.Fatalf("working set was not refreshed: %+v", nm.list.issues)
	}
	visible := filteredIssues(nm.list.issues, nm.list.filter)
	if len(visible) != 1 || visible[0].ShortID != "bbb2" {
		t.Fatalf("render filter did not narrow refreshed working set: %+v", visible)
	}
}

// TestEdge_StaleRefetch_DroppedAcrossScopeToggle: a refetch dispatched
// under single-project scope arrives after the user toggled to all-
// projects. dispatchKey carries the original scope; populateCache
// drops the response so the new scope's list isn't polluted by single-
// project rows.
func TestEdge_StaleRefetch_DroppedAcrossScopeToggle(t *testing.T) {
	m := initialModel(Options{})
	m.api = &Client{}
	// Currently in all-projects scope.
	m.scope = scope{allProjects: true}
	m.list.loading = false
	m.list.issues = []Issue{{UID: "01TEST-99zz", ShortID: "99zz", Title: "all-projects row"}}

	stale := refetchedMsg{
		dispatchKey: cacheKey{projectID: 7}, // single-project at dispatch
		issues:      []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "single-project row"}},
	}
	nm, _ := updateModel(m, stale)
	if len(nm.list.issues) != 1 || nm.list.issues[0].ShortID != "99zz" {
		t.Fatalf("stale single-project refetch leaked into all-projects view: %+v",
			nm.list.issues)
	}
}

// TestQuit_QPressed_OpensConfirm: pressing q opens the M3.5b
// quit-confirm modal instead of immediately quitting. The modal
// owns key dispatch from this point until y/n/esc closes it.
func TestQuit_QPressed_OpensConfirm(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	nm, cmd := updateModel(m, runeKey('q'))
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatal("q produced tea.Quit; should have opened the confirm modal")
			}
		}
	}
	if nm.modal != modalQuitConfirm {
		t.Fatalf("modal = %v, want modalQuitConfirm", nm.modal)
	}
}

// TestQuit_CtrlCFastQuits: ctrl+c bypasses the confirm modal and
// triggers tea.Quit immediately. Power-user escape hatch.
func TestQuit_CtrlCFastQuits(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	nm, cmd := updateModel(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if nm.modal != modalNone {
		t.Fatalf("ctrl+c opened a modal: %v", nm.modal)
	}
	if cmd == nil {
		t.Fatal("ctrl+c produced no cmd; expected tea.Quit")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatalf("ctrl+c cmd = %T, want tea.QuitMsg", cmd())
	}
}

// TestQuit_YConfirms: with the quit modal open, pressing y commits
// — returns tea.Quit.
func TestQuit_YConfirms(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	m.modal = modalQuitConfirm
	_, cmd := m.Update(runeKey('y'))
	if cmd == nil {
		t.Fatal("y in modal produced no cmd; expected tea.Quit")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatalf("y cmd = %T, want tea.QuitMsg", cmd())
	}
}

// TestQuit_NCancels: with the quit modal open, n closes it without
// quitting. Esc behaves the same.
func TestQuit_NCancels(t *testing.T) {
	for _, k := range []rune{'n', 'N'} {
		m := initialModel(Options{})
		m.list.loading = false
		m.modal = modalQuitConfirm
		nm, cmd := updateModel(m, runeKey(k))
		if cmd != nil {
			if msg := cmd(); msg != nil {
				if _, isQuit := msg.(tea.QuitMsg); isQuit {
					t.Fatalf("%q in modal triggered quit; expected cancel", k)
				}
			}
		}
		if nm.modal != modalNone {
			t.Fatalf("%q did not close the modal: %v", k, nm.modal)
		}
	}
	// Esc also cancels.
	m := initialModel(Options{})
	m.list.loading = false
	m.modal = modalQuitConfirm
	nm, _ := updateModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	if nm.modal != modalNone {
		t.Fatal("Esc did not close the quit modal")
	}
}

// TestQuit_ModalAbsorbsOtherKeys: while the quit modal is open,
// unrelated keys (j, /, x) don't reach the underlying view. The
// modal's exclusive routing is what makes "q to confirm" safe.
func TestQuit_ModalAbsorbsOtherKeys(t *testing.T) {
	m := initialModel(Options{})
	m.list.loading = false
	m.modal = modalQuitConfirm
	nm, cmd := updateModel(m, runeKey('j'))
	if cmd != nil {
		t.Fatalf("j during modal returned cmd %T; should be absorbed", cmd)
	}
	if nm.modal != modalQuitConfirm {
		t.Fatal("j during modal closed the modal; expected absorption")
	}
	if nm.list.cursor != 0 {
		t.Fatalf("j during modal moved cursor to %d; expected absorbed", nm.list.cursor)
	}
}

// TestEdge_DetailMutation_StaleGen_MarksCacheStale: a detail-side
// close is in flight; the user jumps to a different issue (gen
// advances). When the original close completes, dm.applyMutation
// would silently drop it on gen mismatch — leaving the list cache
// stale. routeMutation now marks the cache stale so the next list
// refetch picks up the change without waiting for SSE. Regression
// for roborev #89 finding 1.
func TestEdge_DetailMutation_StaleGen_MarksCacheStale(t *testing.T) {
	m := newTestModel()
	m.view = viewDetail
	// Pretend the user is now on issue #99 (gen=10) after jumping
	// from issue #42 (gen=5).
	m.detail.issue = &Issue{ProjectID: 7, UID: "01TEST-99zz", ShortID: "99zz"}
	m.detail.gen = 10
	// Prime the cache so isStale() can detect the stale mark.
	m.cache.put(cacheKey{projectID: 7}, []Issue{
		{UID: "01TEST-42aa", ShortID: "42aa"},
		{UID: "01TEST-99zz", ShortID: "99zz"},
	})

	mut := mutationDoneMsg{
		origin: "detail", gen: 5, kind: "close",
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa"}},
	}
	nm, _ := updateModel(m, mut)
	if !nm.cache.isStale() {
		t.Fatal("stale-gen detail mutation must mark cache stale so next refetch repopulates")
	}
	// dm itself is unchanged — no status churn for the now-current
	// detail issue (#99) from a mutation that targeted #42.
	if nm.detail.status != "" {
		t.Fatalf("detail status should not be churned: %q", nm.detail.status)
	}
}

// TestEdge_JumpDetail_ViewGuard: a queued jumpDetailMsg that arrives
// after the user popped to the list view (or opened help) must NOT
// mutate detail state or dispatch fetches. handleJumpDetail gates on
// view==viewDetail. Regression for roborev #89 finding 2.
func TestEdge_JumpDetail_ViewGuard(t *testing.T) {
	m := newTestModel()
	// User left detail view between the keypress and the jump msg.
	m.view = viewList
	m.detail.issue = &Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa"}
	m.detail.scopePID = 7
	m.detail.gen = 5
	priorGen := m.detail.gen
	priorShortID := m.detail.issue.ShortID

	nm, cmd := updateModel(m, jumpDetailMsg{ref: "99zz"})
	if cmd != nil {
		t.Fatalf("jump while not in viewDetail must dispatch no fetches, got %T", cmd)
	}
	if nm.detail.gen != priorGen {
		t.Fatalf("detail.gen was bumped while not in viewDetail: %d → %d",
			priorGen, nm.detail.gen)
	}
	if nm.detail.issue.ShortID != priorShortID {
		t.Fatalf("detail.issue churned by stale jump: %q → %q",
			priorShortID, nm.detail.issue.ShortID)
	}
}

// TestEdge_FilterChange_ClearsSelectedUID: pressing `s` (cycle
// status) or `c` (clear filters) must reset both cursor AND
// selectedUID without dispatching a refetch. Otherwise the next
// identity-restore can pull the cursor back to the previously-selected
// issue if it survived the new filter, defeating the explicit "I changed
// the filter" intent. Regression for roborev #90 finding 1.
func TestEdge_FilterChange_ClearsSelectedUID(t *testing.T) {
	m := newTestModel()
	m.list.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha", Status: "open"},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "beta", Status: "open"},
		{UID: "01TEST-ccc3", ShortID: "ccc3", Title: "gamma", Status: "open"},
	}
	m.list.cursor = 1
	m.list.selectedUID = "01TEST-bbb2" // cursor on bbb2

	nm, cmd := updateModel(m, runeKey('s'))
	if nm.list.selectedUID != "" {
		t.Fatalf("selectedUID = %q, want empty (filter change clears identity)",
			nm.list.selectedUID)
	}
	if cmd != nil {
		t.Fatalf("status filter change should not dispatch a refetch, got %T", cmd)
	}
}

// TestEdge_ClearFilters_ClearsSelectedUID: same as above for `c`
// (clear filters).
func TestEdge_ClearFilters_ClearsSelectedUID(t *testing.T) {
	m := newTestModel()
	m.list.filter = ListFilter{Status: "open", Owner: "alice"}
	m.list.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "alpha", Status: "open", Owner: ptrString("alice")},
	}
	m.list.cursor = 0
	m.list.selectedUID = "01TEST-aaa1"

	nm, _ := updateModel(m, runeKey('c'))
	if nm.list.selectedUID != "" {
		t.Fatalf("selectedUID = %q, want empty (clear filters resets identity)",
			nm.list.selectedUID)
	}
}

// TestEdge_ListMutation_CompletesAfterDetailOpen: the user closes an
// issue from the list view (mutationDoneMsg origin="list" is in
// flight), then opens a different issue in detail view before the
// mutation completes. When the mutation result lands, it must still
// reach listModel.applyMutation (so the list status line updates and
// the post-success refetch fires) — without top-level routing,
// dispatchToView would forward to detail and the result would be
// silently dropped.
func TestEdge_ListMutation_CompletesAfterDetailOpen(t *testing.T) {
	m := newTestModel()
	m.list.actor = "tester"
	m.list.issues = []Issue{{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "x"}}
	// Simulate having opened detail view after dispatching the close.
	m.view = viewDetail
	m.detail.issue = &Issue{ProjectID: 7, UID: "01TEST-99zz", ShortID: "99zz", Title: "other"}

	mut := mutationDoneMsg{origin: "list", kind: "close",
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-aaa1", ShortID: "aaa1"}}}
	nm, _ := updateModel(m, mut)
	if nm.list.status == "" {
		t.Fatal("list mutation completion was dropped while detail was active")
	}
	if !strings.Contains(nm.list.status, "closed #aaa1") {
		t.Fatalf("list.status = %q, want hint about closed #aaa1", nm.list.status)
	}
}

// TestEdge_DetailMutation_CompletesAfterPopToList: the user closes an
// issue from detail view (mutationDoneMsg origin="detail" in flight),
// then pops back to the list before the mutation completes. The
// result must still reach detailModel.applyMutation so dm.status
// reflects the close and the post-success refetch is dispatched.
// Without top-level routing, dispatchToView would forward to the list
// and the response would be silently dropped.
func TestEdge_DetailMutation_CompletesAfterPopToList(t *testing.T) {
	m := newTestModel()
	// Detail is initialized with a current issue and gen=5 from a recent
	// open; after popping, m.view is viewList but m.detail still holds
	// the prior state until the next open.
	m.detail.issue = &Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "to close"}
	m.detail.scopePID = 7
	m.detail.gen = 5
	m.view = viewList

	mut := mutationDoneMsg{origin: "detail", gen: 5, kind: "close",
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa"}}}
	nm, _ := updateModel(m, mut)
	if nm.detail.status == "" {
		t.Fatal("detail mutation completion was dropped while list was active")
	}
	if !strings.Contains(nm.detail.status, "closed #42aa") {
		t.Fatalf("detail.status = %q, want hint about closed #42aa", nm.detail.status)
	}
}

// TestEdge_DetailJumpBack: open issue A → press Enter on a link to
// jump to B → press Esc to go back. The post-Esc detail must restore A
// verbatim (issue, activeTab, tabCursor) so the user is exactly where
// they left off. Regression for the navStack roundtrip.
//
// Driven through Model.Update so the jumpDetailMsg flow exercises
// Model.handleJumpDetail end-to-end (gen comes from m.nextGen).
func TestEdge_DetailJumpBack(t *testing.T) {
	m := newTestModel()
	m.connGen = 11
	m.view = viewDetail

	// Build A with one link to issue #7. We seed activeTab=tabLinks and
	// tabCursor=0 so Enter has a jump target on the first row.
	original := detailModel{
		issue:     &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "current", Status: "open"},
		scopePID:  7,
		activeTab: tabLinks,
		tabCursor: 0,
		gen:       1,
		links: []LinkEntry{
			{
				ID: 1, Type: "blocks",
				From:   LinkPeer{UID: "01TEST-42aa", ShortID: "42aa"},
				To:     LinkPeer{UID: "01TEST-7bb", ShortID: "7bb"},
				Author: "wesm",
			},
		},
	}
	m.detail = original
	m.nextGen = 1

	// Press Enter on the link → emits jumpDetailMsg(7bb).
	m, cmd := updateModel(m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected jump cmd from Enter")
	}
	jm, ok := cmd().(jumpDetailMsg)
	if !ok || jm.ref != "7bb" {
		t.Fatalf("expected jumpDetailMsg(7bb), got %T (%v)", cmd(), cmd())
	}
	if jm.connGen != 11 {
		t.Fatalf("jumpDetailMsg.connGen = %d, want 11", jm.connGen)
	}

	// Feed the jumpDetailMsg back so Model.handleJumpDetail performs
	// the navStack push and gen advance.
	m, _ = updateModel(m, jm)
	if len(m.detail.navStack) != 1 {
		t.Fatalf("navStack length = %d, want 1", len(m.detail.navStack))
	}
	if m.detail.issue != nil {
		t.Fatalf("expected post-jump dm to be loading (issue=nil), got %+v",
			m.detail.issue)
	}
	if m.detail.gen == original.gen {
		t.Fatal("gen must advance on jump")
	}

	// Apply the in-flight detailFetchedMsg so the stacked view has data.
	m, _ = updateModel(m, detailFetchedMsg{
		gen:   m.detail.gen,
		issue: &Issue{UID: "01TEST-7bb", ShortID: "7bb", Title: "linked target"},
	})
	if m.detail.issue == nil || m.detail.issue.ShortID != "7bb" {
		t.Fatalf("post-fetch dm.issue.ShortID = %v, want 7bb", m.detail.issue)
	}

	// Press Esc → pop to original (handleBack restores from navStack).
	m, popCmd := updateModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	if popCmd != nil {
		t.Fatalf("Esc on stacked detail must not emit a cmd, got %T", popCmd)
	}
	if m.detail.issue == nil || m.detail.issue.ShortID != "42aa" {
		t.Fatalf("post-pop issue = %v, want #42aa (original)", m.detail.issue)
	}
	if m.detail.activeTab != tabLinks {
		t.Fatalf("activeTab not restored: got %d, want tabLinks", m.detail.activeTab)
	}
	if m.detail.tabCursor != 0 {
		t.Fatalf("tabCursor not restored: got %d, want 0", m.detail.tabCursor)
	}
	if len(m.detail.navStack) != 0 {
		t.Fatalf("navStack should be empty after pop, got %d", len(m.detail.navStack))
	}
	// The original issue's links slice should also be intact.
	if len(m.detail.links) != 1 || m.detail.links[0].To.ShortID != "7bb" {
		t.Fatalf("links not restored: %+v", m.detail.links)
	}
}

// TestOverlayModal_PadsShortBackground covers the case where the
// rendered background is shorter than the terminal height (e.g. the
// help view does not pad to height). overlayModal must extend the
// background with blank rows before splicing or the modal lands
// past the last bg line and disappears entirely. Regression for
// roborev #119 finding 1.
func TestOverlayModal_PadsShortBackground(t *testing.T) {
	bg := "row1\nrow2\nrow3" // 3 lines, terminal height = 30
	modal := "[Y]\n[N]"      // 2-line modal — would land near center (~14)
	out := overlayModal(bg, modal, 80, 30)
	lines := strings.Split(out, "\n")
	if len(lines) != 30 {
		t.Fatalf("output line count = %d, want 30 (padded to height)", len(lines))
	}
	containsY := false
	for _, ln := range lines {
		if strings.Contains(ln, "[Y]") {
			containsY = true
			break
		}
	}
	if !containsY {
		t.Fatalf("modal not visible in padded output:\n%s", out)
	}
}

// TestDetail_ScrollIndicator_ViewportCoversWholeDocument: the info
// indicator reports the unified viewport position. With 6 multi-line
// comments plus body and headers, the document is well over a 16-row
// terminal, so the indicator must surface and read in document-line
// units (not per-tab entry units, which the legacy detail view used
// before the unified-viewport refactor).
func TestDetail_ScrollIndicator_ViewportCoversWholeDocument(t *testing.T) {
	cs := make([]CommentEntry, 6)
	body := "wrapped body content that takes a few lines"
	for i := range cs {
		cs[i] = CommentEntry{Author: "actor", Body: body}
	}
	dm := detailModel{
		issue: &Issue{
			UID:     "01TEST-aaa1",
			ShortID: "aaa1",
			Title:   "x",
			Status:  "open",
			Body:    strings.Repeat("body line\n", 12),
		},
		comments:  cs,
		activeTab: tabComments,
		tabCursor: 0,
	}
	docLines, _ := dm.detailDocumentLines(120, viewChrome{})
	const visible = 16
	info := dm.renderInfoLine(120,
		viewChrome{sseStatus: sseConnected}, len(docLines), 0, visible)
	wantSuffix := fmt.Sprintf("of %d]", len(docLines))
	if !strings.Contains(info, wantSuffix) {
		t.Fatalf("indicator missing total; want suffix %q, got info = %q",
			wantSuffix, info)
	}
	if !strings.Contains(info, "lines 1-16 ") {
		t.Fatalf("indicator missing visible range; got info = %q", info)
	}
}
