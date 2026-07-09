package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// layoutTestSetup pins KATA_COLOR_MODE=none and rebuilds the styles
// against io.Discard so View() output is plain UTF-8 (no ANSI). It
// also returns a fresh Model with no loading flag and the listFixture
// seeded so the layout tests have content to render.
func layoutTestSetup(t *testing.T) (Model, func()) {
	t.Helper()
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode()
	m := initialModel(Options{})
	m.list.loading = false
	m.list.issues = snapListFixture()
	cleanup := func() { applyDefaultColorMode() }
	return m, cleanup
}

// TestLayout_PickLayout_Stacked verifies the stacked-fallback branch:
// any width below the breakpoint OR any height below the breakpoint
// must return layoutStacked. The post-Plan-8 thresholds are
// width>=140, height>=36.
func TestLayout_PickLayout_Stacked(t *testing.T) {
	cases := []struct {
		w, h int
	}{
		{100, 40}, // width below threshold
		{160, 30}, // height below threshold
		{139, 36}, // exactly one cell below width
		{140, 35}, // exactly one row below height
	}
	for _, c := range cases {
		if got := pickLayout(c.w, c.h); got != layoutStacked {
			t.Errorf("pickLayout(%d, %d) = %v, want layoutStacked", c.w, c.h, got)
		}
	}
}

// TestLayout_PickLayout_Split verifies the split branch fires when
// BOTH dimensions meet the breakpoint. 140x36 is the minimum split
// terminal; 200x50 is comfortable.
func TestLayout_PickLayout_Split(t *testing.T) {
	cases := []struct {
		w, h int
	}{
		{140, 36}, // exactly at breakpoint
		{160, 40}, // typical wide
		{200, 50}, // very wide
	}
	for _, c := range cases {
		if got := pickLayout(c.w, c.h); got != layoutSplit {
			t.Errorf("pickLayout(%d, %d) = %v, want layoutSplit", c.w, c.h, got)
		}
	}
}

// TestLayout_ResizeSplitToStacked_PreservesSelectionFocusDetail covers
// the split → stacked transition while focusDetail is active. The
// resulting m.view must be viewDetail (the user's focused pane), and
// selectedNumber must survive (identity-based, never touched by the
// layout flip).
func TestLayout_ResizeSplitToStacked_PreservesSelectionFocusDetail(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	// Boot into split layout.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Fatalf("setup failed: layout=%v want split", m.layout)
	}
	// Seed an open detail + focus detail + selectedNumber.
	iss := m.list.issues[1]
	m.detail.issue = &iss
	m.detail.scopePID = 7
	m.focus = focusDetail
	m.list.selectedUID = "01TEST-42aa"
	// Resize down across the breakpoint.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Errorf("layout=%v after resize, want layoutStacked", m.layout)
	}
	if m.view != viewDetail {
		t.Errorf("view=%v after split→stacked focusDetail flip, want viewDetail", m.view)
	}
	if m.list.selectedUID != "01TEST-42aa" {
		t.Errorf("selectedUID=%q after flip, want 01TEST-42aa", m.list.selectedUID)
	}
}

// TestLayout_ResizeSplitToStacked_PreservesSelectionFocusList covers
// the split → stacked transition while focusList is active. The
// resulting m.view must be viewList.
func TestLayout_ResizeSplitToStacked_PreservesSelectionFocusList(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m.focus = focusList
	m.list.selectedUID = "01TEST-99zz"
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Errorf("layout=%v after resize, want layoutStacked", m.layout)
	}
	if m.view != viewList {
		t.Errorf("view=%v after split→stacked focusList flip, want viewList", m.view)
	}
	if m.list.selectedUID != "01TEST-99zz" {
		t.Errorf("selectedUID=%q after flip, want 01TEST-99zz", m.list.selectedUID)
	}
}

// TestLayout_ResizeStackedToSplit_PreservesFocusFromList: stacked
// viewList, resize up to a split-mode-eligible terminal → focus
// follows view (focusList).
func TestLayout_ResizeStackedToSplit_PreservesFocusFromList(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	// Start stacked, viewList.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked || m.view != viewList {
		t.Fatalf("setup failed: layout=%v view=%v", m.layout, m.view)
	}
	// Resize up across the breakpoint.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Errorf("layout=%v after resize, want layoutSplit", m.layout)
	}
	if m.focus != focusList {
		t.Errorf("focus=%v after stacked→split from viewList, want focusList", m.focus)
	}
}

// TestLayout_ResizeStackedToSplit_PreservesFocusFromDetail: stacked
// viewDetail, resize up → focus follows view (focusDetail). Requires
// dm.issue to be set (otherwise focus falls back to focusList).
func TestLayout_ResizeStackedToSplit_PreservesFocusFromDetail(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	iss := m.list.issues[0]
	m.detail.issue = &iss
	m.view = viewDetail
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Errorf("layout=%v after resize, want layoutSplit", m.layout)
	}
	if m.focus != focusDetail {
		t.Errorf("focus=%v after stacked→split from viewDetail, want focusDetail", m.focus)
	}
}

// TestLayout_SplitListPaneWidth_GrowsWithTerminal verifies the list
// pane reclaims space on wide terminals so the title column stays
// readable. The detail pane reserves documentSheetMaxWidth + gutter
// + border (~100 cells) for the document sheet; everything beyond
// that goes to the list pane up to a usability cap.
func TestLayout_SplitListPaneWidth_GrowsWithTerminal(t *testing.T) {
	cases := []struct {
		termWidth int
		want      int
	}{
		{140, 68},  // breakpoint minimum: floor at 68
		{160, 68},  // still floored — detail wants ~100
		{168, 68},  // exact transition: 168-100=68
		{180, 80},  // grows: 180-100
		{200, 100}, // user's terminal in the screenshot
		{220, 110}, // capped at 110
		{300, 110}, // still capped
	}
	for _, c := range cases {
		got := splitListPaneWidth(c.termWidth)
		if got != c.want {
			t.Errorf("splitListPaneWidth(%d) = %d, want %d", c.termWidth, got, c.want)
		}
	}
}

// TestLayout_ToggleLayout_FromSplitToStacked: pressing the layout-
// toggle key in split mode flips to stacked, sets layoutLocked so a
// subsequent WindowSizeMsg cannot auto-flip back, and migrates view
// from focus (mirrors the existing handleLayoutFlip path).
func TestLayout_ToggleLayout_FromSplitToStacked(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	if m.layout != layoutSplit {
		t.Fatalf("setup failed: layout=%v want split", m.layout)
	}
	iss := m.list.issues[0]
	m.detail.issue = &iss
	m.focus = focusDetail
	m = sendRune(m, 'L')
	if m.layout != layoutStacked {
		t.Errorf("layout=%v after L toggle, want layoutStacked", m.layout)
	}
	if !m.layoutLocked {
		t.Error("layoutLocked=false after L toggle, want true")
	}
	if m.view != viewDetail {
		t.Errorf("view=%v after toggle from focusDetail, want viewDetail", m.view)
	}
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 200, Height: 50})
	if m.layout != layoutStacked {
		t.Errorf("layout=%v after resize while locked, want layoutStacked", m.layout)
	}
}

// TestLayout_ToggleLayout_FromStackedToSplit: pressing L in stacked
// mode flips to split when the terminal is large enough.
func TestLayout_ToggleLayout_FromStackedToSplit(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	// Boot in a split-eligible terminal but force-stacked first.
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 160, Height: 40})
	m = sendRune(m, 'L')
	if m.layout != layoutStacked {
		t.Fatalf("setup failed: layout=%v want stacked after first L", m.layout)
	}
	m = sendRune(m, 'L')
	if m.layout != layoutSplit {
		t.Errorf("layout=%v after second L toggle, want layoutSplit", m.layout)
	}
	if !m.layoutLocked {
		t.Error("layoutLocked=false after second L toggle, want true")
	}
}

// TestLayout_ToggleLayout_RefusesSplitOnTooNarrowTerminal: pressing L
// in stacked mode on a terminal too small for split keeps stacked.
// The lock still sticks (so a resize across the threshold can apply
// the user's pref), but the rendered layout stays usable.
func TestLayout_ToggleLayout_RefusesSplitOnTooNarrowTerminal(t *testing.T) {
	m, cleanup := layoutTestSetup(t)
	defer cleanup()
	m, _ = updateModel(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.layout != layoutStacked {
		t.Fatalf("setup failed: layout=%v want stacked", m.layout)
	}
	m = sendRune(m, 'L')
	if m.layout != layoutStacked {
		t.Errorf("layout=%v after L on too-narrow term, want layoutStacked", m.layout)
	}
}
