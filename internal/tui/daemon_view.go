package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

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
	if a.Name != "" && b.Name != "" {
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
		if m.daemonCursor < 0 || m.daemonCursor >= len(rows) {
			return m, nil
		}
		m.daemonSwitchAttempt++
		return m, switchDaemonCmd(rows[m.daemonCursor].target, m.daemonSwitchAttempt)
	}
	return m, nil
}

func switchDaemonCmd(target daemonTarget, attempt uint64) tea.Cmd {
	return func() tea.Msg {
		conn, err := connectDaemonTargetForTUI(context.Background(), target)
		return daemonSwitchResultMsg{attempt: attempt, conn: conn, target: target, err: err}
	}
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

func (m Model) handleDaemonSwitchResult(msg daemonSwitchResultMsg) (Model, tea.Cmd) {
	if msg.attempt != 0 && msg.attempt != m.daemonSwitchAttempt {
		return m, nil
	}
	if msg.err != nil {
		name := sanitizeForLine(daemonTargetDisplay(msg.target))
		errText := sanitizeForLine(msg.err.Error())
		m.toast = &toast{
			text:      "daemon " + quoteForToast(name) + ": " + errText,
			level:     toastError,
			expiresAt: m.toastNow().Add(toastNoBindingTTL),
		}
		return m, toastExpireCmd(toastNoBindingTTL)
	}
	return m.installDaemonConnection(msg.conn)
}

func (m Model) installDaemonConnection(conn daemonConnection) (Model, tea.Cmd) {
	actor := m.list.actor
	m.connGen++
	m.api = conn.api
	m.activeDaemon = conn.target
	if len(conn.catalog) > 0 {
		m.daemonTargets = conn.catalog
	}
	m.scope = conn.init.scope
	m.view = conn.init.view
	m.prevView = viewList
	m.focus = focusList
	m.list = newListModel()
	m.list.actor = actor
	m.detail = newDetailModel()
	m.cache = newIssueCache()
	m.projectLabels = newLabelCache()
	m.projectsByID = map[int64]string{}
	m.projectIdentByID = map[int64]string{}
	m.projectStats = map[int64]ProjectStatsSummary{}
	m.projectsCursor = 0
	m.projectsGen = 0
	m.pendingRefetch = false
	m.projectsStale = false
	m.projectsRefetchPending = false
	m.input = inputState{}
	m.modal = modalNone
	m.sseStatus = sseConnected
	m.nextGen++
	m.nextDetailFollowGen++
	if len(conn.init.projects) > 0 {
		m.projectsByID = make(map[int64]string, len(conn.init.projects))
		m.projectIdentByID = make(map[int64]string, len(conn.init.projects))
		m.projectStats = make(map[int64]ProjectStatsSummary, len(conn.init.projects))
		for _, r := range conn.init.projects {
			m.projectsByID[r.ID] = r.Name
			m.projectIdentByID[r.ID] = r.Name
			if r.Stats != nil {
				m.projectStats[r.ID] = *r.Stats
			}
		}
	}
	var cmds []tea.Cmd
	if m.sseRestart != nil {
		if cmd := m.sseRestart(conn, m.connGen, m.sseCh); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if m.view == viewList && !m.scope.empty {
		cmds = append(cmds, m.fetchInitial(), m.fetchProjects())
	}
	if m.view == viewProjects {
		cmds = append(cmds, m.fetchProjects())
	}
	return m, tea.Batch(cmds...)
}

func quoteForToast(s string) string {
	return `"` + s + `"`
}
