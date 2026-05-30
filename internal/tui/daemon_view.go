package tui

import tea "github.com/charmbracelet/bubbletea"

type daemonRow struct {
	target  daemonTarget
	current bool
}

func daemonRows(targets []daemonTarget, active daemonTarget) []daemonRow {
	rows := make([]daemonRow, 0, len(targets))
	for _, target := range targets {
		rows = append(rows, daemonRow{
			target:  target,
			current: daemonTargetsMatch(target, active),
		})
	}
	return rows
}

func daemonTargetsMatch(a, b daemonTarget) bool {
	if a.Name != "" || b.Name != "" {
		return a.Name == b.Name
	}
	if a.Local || b.Local {
		return a.Local == b.Local
	}
	return a.URL == b.URL
}

func (m Model) transitionToDaemons() (Model, tea.Cmd) {
	if len(m.daemonTargets) == 0 {
		return m, nil
	}
	m.prevView = m.view
	m.view = viewDaemons
	m.daemonCursor = cursorForDaemon(daemonRows(m.daemonTargets, m.activeDaemon))
	return m, nil
}

func (m Model) routeDaemonsViewKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := daemonRows(m.daemonTargets, m.activeDaemon)
	if next, ok := m.cursorMoveDaemons(msg, rows); ok {
		return next, nil
	}
	switch msg.String() {
	case "esc":
		return m.escFromDaemonsView()
	case "enter":
		return m, nil
	}
	return m, nil
}

func (m Model) cursorMoveDaemons(msg tea.KeyMsg, rows []daemonRow) (Model, bool) {
	switch msg.String() {
	case "j", "down":
		if m.daemonCursor < len(rows)-1 {
			m.daemonCursor++
		}
		return m, true
	case "k", "up":
		if m.daemonCursor > 0 {
			m.daemonCursor--
		}
		return m, true
	case "g", "home":
		m.daemonCursor = 0
		return m, true
	case "G", "end":
		m.daemonCursor = len(rows) - 1
		return m, true
	}
	return m, false
}

func (m Model) escFromDaemonsView() (Model, tea.Cmd) {
	if m.prevView == viewDaemons {
		m.view = viewList
		return m, nil
	}
	m.view = m.prevView
	return m, nil
}

func cursorForDaemon(rows []daemonRow) int {
	for i, row := range rows {
		if row.current {
			return i
		}
	}
	return 0
}
