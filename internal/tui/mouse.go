package tui

import tea "github.com/charmbracelet/bubbletea"

const (
	stackedListDataRowY   = 4
	splitListDataRowY     = 4
	projectsFirstRowY     = 4
	mouseWheelScrollLines = 3
)

func (m Model) routeMouse(msg tea.MouseMsg) (Model, tea.Cmd) {
	if !m.opts.Mouse || msg.Action != tea.MouseActionPress || !m.canQuit() {
		return m, nil
	}
	if !m.mouseVisibleViewAcceptsInput() {
		return m, nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		return m.mouseWheelAt(-1, msg.X)
	case tea.MouseButtonWheelDown:
		return m.mouseWheelAt(1, msg.X)
	case tea.MouseButtonLeft:
		return m.mouseLeftClick(msg.X, msg.Y)
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
	if m.layout == layoutSplit {
		if x >= splitListPaneWidth(m.width) {
			m.focus = focusDetail
			return m.mouseDetailWheel(delta), nil
		}
		m.focus = focusList
		return m.mouseListWheel(delta)
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
	if m.layout == layoutSplit {
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
	if m.layout == layoutSplit {
		if x < splitListPaneWidth(m.width) {
			m.focus = focusList
			return m.mouseListClick(splitListRowY(y))
		}
		m.focus = focusDetail
		return m, nil
	}
	if m.view == viewList {
		return m.mouseListClick(y - stackedListDataRowY)
	}
	return m, nil
}

func splitListRowY(y int) int {
	// Title, pane top border, table header, separator, then first data row.
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
	if m.layout == layoutSplit {
		return m.scheduleDetailFollow()
	}
	return m, nil
}

func (m Model) listDataBudget() int {
	if m.layout == layoutSplit {
		footerLines := helpLines(m.splitHelpRows(), m.width)
		bodyHeight := m.height - 2 - footerLines
		if bodyHeight < 4 {
			bodyHeight = 4
		}
		innerH := bodyHeight - 2
		if innerH < 2 {
			innerH = 2
		}
		return innerH - 2
	}
	footerLines := helpLines(listHelpRows(m.list, m.chrome()), m.width)
	bodyRows := m.height - 2 - 1 - footerLines
	if bodyRows < listBodyFloor {
		bodyRows = listBodyFloor
	}
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
		budget = m.height - projectsViewChromeRows(footerLines)
		if budget < 1 {
			budget = 1
		}
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
