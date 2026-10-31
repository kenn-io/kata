package tui

import (
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kit/tui/splitlayout"
)

const (
	stackedListDataRowY   = 4
	splitListDataRowY     = 4
	projectsFirstRowY     = 4
	mouseWheelScrollLines = 3
)

// routeMouse handles the press-shaped mouse messages: wheel steps and
// left clicks. Bubble Tea v2 splits tea.MouseMsg into per-action types
// (click/release/wheel/motion), so the v1 Action==Press filter is now
// the type switch itself — release and motion messages fall through
// untouched.
func (m Model) routeMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if !m.opts.Mouse {
		return m, nil
	}
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft &&
		m.modal == modalNone && (m.input.kind == inputNone || m.input.kind == inputSearchBar) &&
		m.width >= 80 {
		mouse := click.Mouse()
		if m.view == viewCron && !m.cron.help && m.cron.form == nil && len(m.cron.stack) == 0 &&
			m.scope.projectID != 0 && mouse.Y == 2 {
			m.inboxPending = false
			m.cron.filtering = true
			return m, nil
		}
		row := 1
		if m.layout == splitlayout.Split {
			row = 2
		}
		if (m.view == viewList || m.view == viewDetail && m.layout == splitlayout.Split) &&
			mouse.Y == row && (m.layout != splitlayout.Split || mouse.X < splitConfig.ListWidth(m.width)) {
			m.inboxPending = false
			if m.list.pendingPriority {
				m.list.pendingPriority = false
				m.list.status = ""
			}
			m.focus = focusList
			if m.input.kind == inputSearchBar {
				m.input.searchFocus, m.input.searchSubmitted = searchFocusQuery, false
				return m, nil
			}
			m, fetch := m.prepareAttentionListNavigation()
			m.view = viewList
			m, open := m.openInput(inputSearchBar)
			return m, tea.Batch(fetch, open)
		}
	}
	if !m.canQuit() {
		return m, nil
	}
	if !m.mouseVisibleViewAcceptsInput() {
		return m, nil
	}
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		mouse := msg.Mouse()
		switch mouse.Button {
		case tea.MouseWheelUp:
			return m.mouseWheelAt(-1, mouse.X)
		case tea.MouseWheelDown:
			return m.mouseWheelAt(1, mouse.X)
		}
	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		if mouse.Button == tea.MouseLeft {
			return m.mouseLeftClick(mouse.X, mouse.Y)
		}
	}
	return m, nil
}

func (m Model) mouseVisibleViewAcceptsInput() bool {
	switch m.view {
	case viewList, viewDetail, viewProjects, viewFederation:
		return true
	}
	return false
}

func (m Model) mouseWheelAt(delta, x int) (Model, tea.Cmd) {
	if m.view == viewProjects {
		m.moveProjectsCursor(delta)
		return m, nil
	}
	if m.view == viewFederation {
		m.moveFederationCursor(delta)
		return m, nil
	}
	if m.layout == splitlayout.Split {
		if x >= splitConfig.ListWidth(m.width) {
			m.focus = focusDetail
			return m.mouseDetailWheel(delta), nil
		}
		m, browseCmd := m.prepareAttentionListNavigation()
		m.focus = focusList
		next, listCmd := m.mouseListWheel(delta)
		return next, combineCmds(browseCmd, listCmd)
	}
	if m.view == viewDetail {
		return m.mouseDetailWheel(delta), nil
	}
	return m.mouseListWheel(delta)
}

func (m Model) mouseDetailWheel(delta int) Model {
	step := mouseWheelScrollLines
	if delta < 0 {
		step = -step
	}
	m.detail = m.detail.scrollViewportBy(step)
	return m
}

func (m Model) mouseListWheel(delta int) (Model, tea.Cmd) {
	if delta < 0 && m.list.cursor > 0 {
		m.list.cursor--
	}
	if delta > 0 && m.list.cursor < len(m.list.visibleRows())-1 {
		m.list.cursor++
	}
	m = m.applyListViewportCache()
	m.list = m.list.ensureCursorVisible(len(m.list.visibleRows()))
	m.list = m.list.syncSelection(m.list.visibleRows())
	if m.layout == splitlayout.Split {
		return m.scheduleDetailFollow()
	}
	return m, nil
}

func (m Model) mouseLeftClick(x, y int) (Model, tea.Cmd) {
	if m.view == viewProjects {
		return m.mouseProjectsClick(y)
	}
	if m.view == viewFederation {
		return m.mouseFederationClick(y)
	}
	if m.layout == splitlayout.Split {
		if x < splitConfig.ListWidth(m.width) {
			m, browseCmd := m.prepareAttentionListNavigation()
			m.focus = focusList
			next, listCmd := m.mouseListClick(splitListRowY(y))
			return next, combineCmds(browseCmd, listCmd)
		}
		m.focus = focusDetail
		return m, nil
	}
	if m.view == viewList {
		if m.height < listMinHeight {
			return m.mouseListClick(y - 3) // Title, search and table header precede compact rows.
		}
		return m.mouseListClick(y - stackedListDataRowY)
	}
	return m, nil
}

func splitListRowY(y int) int {
	// Title, pane top border, search/stats line, table header, then first data row.
	return y - splitListDataRowY
}

func (m Model) mouseListClick(row int) (Model, tea.Cmd) {
	if row < 0 {
		return m, nil
	}
	rows := m.list.visibleRows()
	if len(rows) == 0 {
		return m, nil
	}
	budget := m.listDataBudget()
	start, end := m.list.windowBounds(len(rows), budget)
	idx := start + row
	if idx < start || idx >= end || idx >= len(rows) {
		return m, nil
	}
	m.list.cursor = idx
	m.list = m.list.syncSelection(rows)
	if m.layout == splitlayout.Split {
		return m.scheduleDetailFollow()
	}
	return m, nil
}

func (m Model) listDataBudget() int {
	if m.layout == splitlayout.Split {
		innerH := max(m.splitGeometry().ListInnerH, 2)
		return innerH - 2
	}
	if m.height < listMinHeight {
		return m.listRenderedDataRows()
	}
	footerLines := helpLines(listHelpRows(m.list, m.chrome()), m.width)
	bodyRows := max(m.height-2-1-footerLines, listBodyFloor)
	return bodyRows - 2
}

func (m Model) mouseProjectsClick(y int) (Model, tea.Cmd) {
	row := y - projectsFirstRowY
	if row < 0 {
		return m, nil
	}
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	budget := len(rows)
	if m.height > 0 {
		footerLines := helpLines(projectsHelpRows(), m.width)
		budget = max(m.height-projectsViewChromeRows(footerLines), 1)
	}
	visible := clipProjectsRows(rows, m.projectsCursor, budget)
	if row >= len(visible) {
		return m, nil
	}
	m.projectsCursor = visible[row].index
	return m, nil
}

func (m *Model) moveProjectsCursor(delta int) {
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	if len(rows) == 0 {
		m.projectsCursor = 0
		return
	}
	m.projectsCursor += delta
	if m.projectsCursor < 0 {
		m.projectsCursor = 0
	}
	if m.projectsCursor >= len(rows) {
		m.projectsCursor = len(rows) - 1
	}
}
