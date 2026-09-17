package tui

import (
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/splitlayout"
)

// focusPane names which pane owns key dispatch in split layout. Only
// meaningful when m.layout == splitlayout.Split; in stacked layout m.view
// is the authoritative dispatch state. Tab/Enter from focusList →
// focusDetail; Esc from focusDetail → focusList.
type focusPane int

const (
	focusList focusPane = iota
	focusDetail
)

var splitConfig = splitlayout.Config{
	ListMinWidth:        68,
	ListMaxWidth:        110,
	DetailReservedWidth: 100,
	DetailMinWidth:      20,
	MinBodyHeight:       4,
}

func (m Model) splitGeometry() splitlayout.Geom {
	return splitConfig.Geometry(m.width, m.height, helpLines(m.splitHelpRows(), m.width))
}

// resolveLayout returns the splitlayout.Mode the model should render for
// its current width/height + lock state. When unlocked, defers to
// splitlayout.PickLayout. When locked, honors preferredLayout but degrades to
// stacked if the terminal cannot fit split — the lock represents
// intent, not a guarantee that split fits.
//
// preferredLayout is read here (not m.layout) so a prior degraded
// resize that pushed m.layout to stacked does not silently erase a
// locked split preference: when the terminal is wide enough again,
// resolveLayout returns splitlayout.Split because preferredLayout still
// says split.
func (m Model) resolveLayout() splitlayout.Mode {
	if !m.layoutLocked {
		return splitlayout.PickLayout(m.width, m.height)
	}
	if m.preferredLayout == splitlayout.Split && splitlayout.PickLayout(m.width, m.height) == splitlayout.Stacked {
		return splitlayout.Stacked
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
	if m.layout == splitlayout.Split {
		m.preferredLayout = splitlayout.Stacked
	} else {
		m.preferredLayout = splitlayout.Split
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
// splitlayout.PickLayout returns a different mode than m.layout had before.
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
func (m Model) handleLayoutFlip(prev splitlayout.Mode) (Model, tea.Cmd) {
	if prev == splitlayout.Split && m.layout == splitlayout.Stacked {
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
	if prev == splitlayout.Stacked && m.layout == splitlayout.Split {
		// Entering split: derive focus from the view the user was
		// looking at. viewHelp / viewEmpty fall through to focusList
		// (the right-hand pane is informational; the list is what
		// they should be navigating).
		if m.view == viewDetail && m.detail.issue != nil {
			m.focus = focusDetail
		} else {
			m.focus = focusList
		}
		// Capture inherited detail before an active search takes ownership.
		// Search must follow the filtered highlight even when a hidden detail
		// is already populated; the ordinary nil-only bootstrap remains the
		// right behavior for non-search layout transitions.
		m = m.captureSearchSplitDetail()
		if m.input.kind.isCommandBar() {
			m, cmd := m.followSearchResultIfNeeded(nil)
			return m.markSearchSplitDetailOwned(), cmd
		}
		// Bootstrap the detail pane on the first stacked→split flip so
		// a launch that landed before the size msg, or a runtime
		// widen/resize/toggle, populates the right pane without
		// requiring a j/k nudge.
		m, cmd := m.maybeBootstrapSplitDetail()
		return m.markSearchSplitDetailOwned(), cmd
	}
	return m, nil
}
