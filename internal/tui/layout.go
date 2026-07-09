package tui

import tea "charm.land/bubbletea/v2"

// layoutMode discriminates between the stacked single-view layout
// (the M1-M5 default) and the M6 split-pane layout that renders the
// list and detail side-by-side. Re-evaluated on every WindowSizeMsg
// via pickLayout so a resize across the breakpoint flips the layout
// in both directions.
type layoutMode int

const (
	layoutStacked layoutMode = iota
	layoutSplit
)

// focusPane names which pane owns key dispatch in split layout. Only
// meaningful when m.layout == layoutSplit; in stacked layout m.view
// is the authoritative dispatch state. Tab/Enter from focusList →
// focusDetail; Esc from focusDetail → focusList.
type focusPane int

const (
	focusList focusPane = iota
	focusDetail
)

// splitListPaneMinWidth is the floor on the list pane's cell width
// in split layout. Plan 7 said 60-64; bumped to 68 for Plan 8 row
// chips so the title column still has 20+ cells after the fixed
// columns (#/status/kids/updated, which sum to ~42 cells in narrow
// mode). Wider terminals grow past this floor — see splitListPaneWidth.
const splitListPaneMinWidth = 68

// splitListPaneMaxWidth caps the list pane on very wide terminals.
// Beyond this point the title column is comfortable enough that
// further growth would just steal cells from the detail pane.
const splitListPaneMaxWidth = 110

// splitDetailPaneReservedWidth is the budget the detail pane needs
// for its document sheet (documentSheetMaxWidth=96 + gutter=2 +
// border=2). Set as a constant rather than referenced symbolically
// to keep this package's layout math independent of detail_render
// (which would otherwise create a coupling cycle for split-mode
// width computations).
const splitDetailPaneReservedWidth = 100

// splitListPaneWidth is the cell width of the list pane in split
// layout. The detail pane gets first dibs on the document sheet's
// reserved budget; everything beyond that goes to the list pane,
// floored at splitListPaneMinWidth and capped at splitListPaneMaxWidth.
//
// At terminal width 140 (the split breakpoint) the list pane sits on
// its floor of 68 cells. As the terminal grows past ~168 cells, the
// list pane reclaims width to give the title column more room.
func splitListPaneWidth(termWidth int) int {
	w := termWidth - splitDetailPaneReservedWidth
	if w < splitListPaneMinWidth {
		return splitListPaneMinWidth
	}
	if w > splitListPaneMaxWidth {
		return splitListPaneMaxWidth
	}
	return w
}

// splitMinWidth and splitMinHeight are the breakpoint thresholds for
// split layout. Below either dimension we fall back to layoutStacked
// so a too-tight terminal keeps the single-pane layout.
//
// User-confirmed thresholds (post Plan-8 chrome bump from 7 to 9
// fixed rows on detail, 5 fixed rows on list): width>=140 (68-cell
// list pane + a usable detail pane) AND height>=36 (9 fixed rows of
// detail chrome + comfortable body content per pane).
const (
	splitMinWidth  = 140
	splitMinHeight = 36
)

// pickLayout chooses between the stacked single-view layout and the
// split-pane layout based on terminal dimensions. Below either
// threshold, fall back to stacked. Re-run on every WindowSizeMsg
// when the user has not manually locked a layout.
func pickLayout(width, height int) layoutMode {
	if width >= splitMinWidth && height >= splitMinHeight {
		return layoutSplit
	}
	return layoutStacked
}

// canRenderSplit reports whether the terminal is large enough to
// render split-pane layout without producing unusable UI. Used both
// by the auto-pick path and by the manual-toggle path so a locked
// split-preference still degrades to stacked when the terminal is
// outright too narrow.
func canRenderSplit(width, height int) bool {
	return width >= splitMinWidth && height >= splitMinHeight
}

// resolveLayout returns the layoutMode the model should render for
// its current width/height + lock state. When unlocked, defers to
// pickLayout. When locked, honors preferredLayout but degrades to
// stacked if the terminal cannot fit split — the lock represents
// intent, not a guarantee that split fits.
//
// preferredLayout is read here (not m.layout) so a prior degraded
// resize that pushed m.layout to stacked does not silently erase a
// locked split preference: when the terminal is wide enough again,
// resolveLayout returns layoutSplit because preferredLayout still
// says split.
func (m Model) resolveLayout() layoutMode {
	if !m.layoutLocked {
		return pickLayout(m.width, m.height)
	}
	if m.preferredLayout == layoutSplit && !canRenderSplit(m.width, m.height) {
		return layoutStacked
	}
	return m.preferredLayout
}

// toggleLayout flips the user's layout preference, sets layoutLocked
// so subsequent WindowSizeMsgs honor it, and runs handleLayoutFlip
// so view/focus migrate consistently with the auto-flip path.
//
// The flip is computed against the EFFECTIVE rendered layout
// (m.layout), not the previously-stored preferredLayout: pressing L
// is the user reacting to what they currently see. Once locked, the
// rendered layout may degrade to stacked if the terminal is too
// narrow, but preferredLayout retains the user's intent so a wider
// resize restores the chosen split layout (roborev #17173 finding 1).
func (m Model) toggleLayout() (Model, tea.Cmd) {
	prev := m.layout
	m.layoutLocked = true
	if m.layout == layoutSplit {
		m.preferredLayout = layoutStacked
	} else {
		m.preferredLayout = layoutSplit
	}
	m.layout = m.resolveLayout()
	var flipCmd tea.Cmd
	if prev != m.layout {
		m, flipCmd = m.handleLayoutFlip(prev)
	}
	// The layout flip changes which footer help-row table is rendered
	// and whether the detail pane is full-width or boxed in a split
	// pane, both of which feed the viewport-dim calculation. Refresh
	// the cache so PgUp/PgDn paging and EOF clamping use the new
	// dimensions immediately, not the stale ones from before the flip.
	m.detail = m.applyDetailViewportCache(m.detail)
	return m, flipCmd
}

// handleLayoutFlip preserves selection and focus across a layout
// transition. Called from routeTopLevel's WindowSizeMsg branch when
// pickLayout returns a different mode than m.layout had before.
//
// stacked → split: derive m.focus from m.view (viewList → focusList,
// viewDetail → focusDetail) so the user's currently-focused pane
// stays the active one in the split. m.view stays as it was so any
// subsequent split → stacked flip restores the same single-pane
// rendering.
//
// split → stacked: set m.view from m.focus (focusList → viewList,
// focusDetail → viewDetail) so the user keeps seeing the pane they
// were last focused on. m.focus stays as it was so a subsequent
// stacked → split flip lands on the same pane.
//
// Selection survives in both directions because lm.selectedNumber is
// identity-based and dm.issue is a pointer the layout flip never
// touches. Other invariants (gen counters, formGen, modal state,
// SSE state) live on Model and are likewise untouched.
func (m Model) handleLayoutFlip(prev layoutMode) (Model, tea.Cmd) {
	if prev == layoutSplit && m.layout == layoutStacked {
		// Coming back to the stacked layout: pick the view that
		// matches the focused pane so the user keeps seeing the
		// pane they last interacted with.
		if m.focus == focusDetail && m.detail.issue != nil {
			m.view = viewDetail
		} else {
			m.view = viewList
		}
		return m, nil
	}
	if prev == layoutStacked && m.layout == layoutSplit {
		// Entering split: derive focus from the view the user was
		// looking at. viewHelp / viewEmpty fall through to focusList
		// (the right-hand pane is informational; the list is what
		// they should be navigating).
		if m.view == viewDetail && m.detail.issue != nil {
			m.focus = focusDetail
		} else {
			m.focus = focusList
		}
		// Bootstrap the detail pane on the first stacked→split flip so
		// a launch that landed before the size msg, or a runtime
		// widen/resize/toggle, populates the right pane without
		// requiring a j/k nudge.
		return m.maybeBootstrapSplitDetail()
	}
	return m, nil
}
