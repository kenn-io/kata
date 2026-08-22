package tui

import (
	"reflect"
	"strings"
	"testing"
)

// TestHelpSections_AllBindingsCovered guards against drift between
// keymap.go and helpSections: if a future task adds a key field, the
// test fails until helpSections also references it. The check counts
// occurrences of each rendered display string so duplicate-bound keys
// (e.g. Open and JumpRef both bind "enter", ClearFilters and NewComment
// both bind "c") all stay covered — dropping one half of a duplicate
// pair leaves a count gap that the assertion catches. The unchecked
// type assertion is replaced by a guarded form so future non-key fields
// on keymap (e.g. a config struct) wouldn't panic the test.
func TestHelpSections_AllBindingsCovered(t *testing.T) {
	km := newKeymap()
	found := map[string]int{}
	for _, s := range helpSections(km) {
		for _, r := range s.rows {
			found[r.key]++
		}
	}
	required := map[string]int{}
	v := reflect.ValueOf(km)
	for _, field := range v.Fields() {
		k, ok := reflect.TypeAssert[key](field)
		if !ok {
			continue
		}
		required[keyDisplay(k)]++
	}
	for display, want := range required {
		if got := found[display]; got < want {
			t.Errorf("display %q: helpSections has %d, keymap requires %d",
				display, got, want)
		}
	}
}

// TestRenderHelp_ChildrenSectionMatchesKeymap pins the Children
// section of the help overlay against the actual j/k vs ↑↓ split:
// j/k moves the child cursor, ↑↓ scrolls the detail viewport. The
// help text used to claim "↑↓ move child cursor" which became stale
// when the document-viewport keymap split landed.
func TestRenderHelp_ChildrenSectionMatchesKeymap(t *testing.T) {
	out := stripANSI(renderHelp(newKeymap(), 80, ListFilter{}))
	if !strings.Contains(out, "j/k") || !strings.Contains(out, "move child cursor") {
		t.Errorf("help overlay should describe j/k as the child-cursor key; got:\n%s", out)
	}
	if !strings.Contains(out, "↑↓") || !strings.Contains(out, "scroll detail viewport") {
		t.Errorf("help overlay should describe ↑↓ as the viewport scroll key; got:\n%s", out)
	}
}

// TestRenderHelp_NarrowWidth: width 40 picks a 1-column layout. We
// assert each section title appears on its own line so a future
// regression that drops Detail (or any other section) is caught.
func TestRenderHelp_NarrowWidth(t *testing.T) {
	out := renderHelp(newKeymap(), 40, ListFilter{})
	assertContainsAll(t, out, "Global", "Graph", "Detail", "Children", "Forms", "Filters")
	if helpColumnCount(40) != 1 {
		t.Fatalf("helpColumnCount(40)=%d, want 1", helpColumnCount(40))
	}
}

// TestRenderHelp_WideWidth: at width 130 the layout uses 3 columns so
// sections lay out side-by-side. We don't assert exact placement (column
// padding varies), but the column count helper is the contract.
func TestRenderHelp_WideWidth(t *testing.T) {
	if helpColumnCount(130) != 3 {
		t.Fatalf("helpColumnCount(130)=%d, want 3", helpColumnCount(130))
	}
	out := renderHelp(newKeymap(), 130, ListFilter{})
	assertContainsAll(t, out,
		"Global", "Graph", "Detail", "Children", "Forms", "Filters", "kata — keybindings",
	)
}

// TestRenderHelp_FilterChips: an active filter renders as a chip strip
// above the bindings so the user can see why their list looks the way
// it does without leaving the help view.
func TestRenderHelp_FilterChips(t *testing.T) {
	out := renderHelp(newKeymap(), 100, ListFilter{Status: "open"})
	if !strings.Contains(out, "status:open") {
		t.Errorf("expected status chip in help output\n%s", out)
	}
}

// TestHelpToggle_FromList_AndBack: pressing ? in viewList enters
// viewHelp; pressing ? again restores viewList. The Model's prevView is
// the carrier so a future viewDetail-and-back would round-trip the same
// way (see TestHelpToggle_FromDetail).
func TestHelpToggle_FromList_AndBack(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewList
	mh := sendRune(m, '?')
	if mh.view != viewHelp {
		t.Fatalf("after ? from list, view = %v, want viewHelp", mh.view)
	}
	ml := sendRune(mh, '?')
	if ml.view != viewList {
		t.Fatalf("after ? from help, view = %v, want viewList", ml.view)
	}
}

// TestHelpToggle_FromDetail: pressing ? in viewDetail enters viewHelp,
// pressing ? again returns to viewDetail (not viewList). Catches a
// regression that would always pop back to the list.
func TestHelpToggle_FromDetail(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewDetail
	mh := sendRune(m, '?')
	if mh.view != viewHelp {
		t.Fatalf("after ? from detail, view = %v, want viewHelp", mh.view)
	}
	md := sendRune(mh, '?')
	if md.view != viewDetail {
		t.Fatalf("after ? from help, view = %v, want viewDetail", md.view)
	}
}

// TestHelpToggle_QuitFromHelp: q from viewHelp opens the M3.5b
// quit-confirm modal instead of immediately quitting. The plan keeps
// q wired to a quit path even inside the overlay so the user can
// always escape regardless of which view is active — but post-M3.5b
// the user has to confirm via the modal first.
func TestHelpToggle_QuitFromHelp(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewHelp
	nm := sendRune(m, 'q')
	if nm.modal != modalQuitConfirm {
		t.Fatalf("q from help did not open quit-confirm modal: %v", nm.modal)
	}
}

// TestHelp_GatedByInputting: pressing ? while the M3a inline command
// bar is open must reach the bar's textinput buffer instead of
// opening help. canQuit gates the global keys via m.input.kind.
func TestHelp_GatedByInputting(t *testing.T) {
	m := initialModel(Options{})
	m.input = newSearchBar(ListFilter{})
	nm := sendRune(m, '?')
	if nm.view == viewHelp {
		t.Fatal("? opened help while bar was active; should be gated")
	}
	if v := nm.input.activeField().value(); v != "?" {
		t.Fatalf("bar buffer = %q, want %q (rune must reach prompt)", v, "?")
	}
}

// TestHelp_RefetchWhileOpen_KeepsListInSync: a refetchedMsg landing
// while the help overlay is active must update lm.issues so toggling
// back to the list does not show stale rows. Pre-fix, dispatchToView
// only forwarded to viewList/viewDetail, so the refetch updated the
// cache but left lm.issues at the pre-help snapshot. The fix moves
// applyFetched into populateCache so cache and list stay in lockstep
// regardless of the active view.
func TestHelp_RefetchWhileOpen_KeepsListInSync(t *testing.T) {
	m := initialModel(Options{})
	m.scope = scope{projectID: 1}
	m.list.issues = []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "old"}}
	m.prevView = viewList
	m.view = viewHelp
	nm, _ := updateModel(m, refetchedMsg{
		dispatchKey: cacheKey{projectID: 1, limit: queueFetchLimit},
		issues:      []Issue{{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "new"}},
	})
	if got := len(nm.list.issues); got != 1 {
		t.Fatalf("list.issues len = %d, want 1", got)
	}
	if nm.list.issues[0].ShortID != "bbb2" || nm.list.issues[0].Title != "new" {
		t.Fatalf("list.issues = %+v, want [{ShortID:bbb2 Title:new}]", nm.list.issues)
	}
	// Toggling back to the list must surface the refreshed rows.
	back := sendRune(nm, '?')
	if back.view != viewList {
		t.Fatalf("after ? from help, view = %v, want viewList", back.view)
	}
	if back.list.issues[0].ShortID != "bbb2" {
		t.Fatal("returning to list must show refetched issues, not stale snapshot")
	}
}

// TestHelp_InitialFetchAfterScopeToggle_KeepsListInSync: pressing R
// (scope toggle) while the help overlay is open dispatches a fresh
// fetchInitial that lands as initialFetchMsg. populateCache must apply
// it to m.list at the top level so toggling back to the list shows
// the new scope's rows. The earlier regression covered refetchedMsg
// only; this exercises the initialFetchMsg path of the same bug.
func TestHelp_InitialFetchAfterScopeToggle_KeepsListInSync(t *testing.T) {
	m := initialModel(Options{})
	m.scope = scope{projectID: 1, homeProjectID: 1, homeProjectName: "home"}
	m.list.issues = []Issue{{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "single-project"}}
	m.prevView = viewList
	m.view = viewHelp
	// Simulate an initialFetchMsg from a scope-toggle's fetchInitial.
	nm, _ := updateModel(m, initialFetchMsg{
		dispatchKey: cacheKey{projectID: 1, limit: queueFetchLimit},
		issues:      []Issue{{UID: "01TEST-99zz", ShortID: "99zz", Title: "all-projects row"}},
	})
	if got := len(nm.list.issues); got != 1 || nm.list.issues[0].ShortID != "99zz" {
		t.Fatalf("list.issues = %+v, want [{ShortID:99zz ...}]", nm.list.issues)
	}
	// Closing the overlay must surface the refreshed rows.
	back := sendRune(nm, '?')
	if back.view != viewList {
		t.Fatalf("after ? from help, view = %v, want viewList", back.view)
	}
	if back.list.issues[0].ShortID != "99zz" {
		t.Fatal("returning to list must show post-toggle rows, not stale snapshot")
	}
}
