package cron

import tea "charm.land/bubbletea/v2"

func (m *model) routeKey(k tea.KeyPressMsg) tea.Cmd {
	if cmd, handled := m.formKey(k); handled {
		return cmd
	}
	if m.searching {
		return m.searchKey(k)
	}
	if m.currentView() == viewWorkflow {
		return m.workflowDetailKey(k)
	}
	return m.routeScreenKey(k)
}
func (m *model) routeScreenKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if cmd, handled := m.definitionKey(k); handled {
		return cmd
	}
	switch {
	case matchesKey(s, keys.Quit, keys.Interrupt):
		return tea.Quit
	case matchesKey(s, keys.Reload):
		return m.refresh()
	case matchesKey(s, keys.Down, keys.ScrollDown):
		m.move(1)
	case matchesKey(s, keys.Up, keys.ScrollUp):
		m.move(-1)
	case matchesKey(s, keys.NextPage, keys.NextPageAlt):
		if m.currentView() == viewList && m.tab == 1 && m.data.MoreRuns && m.cursor+m.pageRows() >= len(m.rows) {
			return func() tea.Msg { return MoreRunsMsg{} }
		}
		m.move(m.pageRows())
	case matchesKey(s, keys.PrevPage, keys.PrevPageAlt):
		m.move(-m.pageRows())
	case matchesKey(s, keys.PageDown, keys.HalfDown):
		m.scroll += m.pageSize()
		m.follow = false
	case matchesKey(s, keys.PageUp, keys.HalfUp):
		m.scroll = max(0, m.scroll-m.pageSize())
		m.follow = false
	case matchesKey(s, keys.Back, keys.Left, keys.Cancel, keys.Backspace):
		if m.currentView() != viewList {
			m.popDetail()
		} else {
			m.query = ""
			m.rebuild()
		}
	case matchesKey(s, keys.Open, keys.Right, keys.Accept):
		return m.open()
	case matchesKey(s, keys.Search):
		if m.currentView() == viewList {
			m.searching = true
		}
	case matchesKey(s, keys.Layout):
		_, split := m.splitGeometry()
		m.listOnly = split
	case matchesKey(s, keys.Toggle):
		if r, ok := m.selected(); ok && r.kind == "workflow" && m.currentView() == viewList && m.workflowExpandable(r) {
			m.expanded[workflowExpandKey(r)] = !m.expanded[workflowExpandKey(r)]
			m.follow = true
		}
	case matchesKey(s, keys.Kata, keys.KataTab):
		return tabCmd(0)
	case matchesKey(s, keys.JobsTab, keys.RunsTab, keys.WorkflowsTab):
		return tabCmd(int(s[0] - '1'))
	case matchesKey(s, keys.NextTab):
		return tabCmd((m.tab + 2) % 4)
	case matchesKey(s, keys.PrevTab):
		return tabCmd(m.tab)
	case matchesKey(s, keys.Issue):
		if r, ok := m.selected(); ok {
			if r.kind == "run" {
				run := m.data.Runs[r.run]
				return m.openIssue(run.ProjectKey, value(run.IssueUID))
			}
			if r.kind == "job" {
				if run, ok := m.selectedJobRun(); ok && value(run.IssueUID) != "" {
					return m.openIssue(run.ProjectKey, value(run.IssueUID))
				}
				if v := m.indexedJob(r); v != nil {
					return m.openIssue(r.projectUID, jobIssueUID(v.def))
				}
			}
			if r.kind == "workflow" {
				if run, ok := m.nestedRun(); ok {
					r := m.data.Runs[run.run]
					return m.openIssue(r.ProjectKey, value(r.IssueUID))
				}
			}
		}
	}
	return nil
}
func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (m *model) searchKey(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()

	switch {
	case matchesKey(s, keys.Interrupt):
		return tea.Quit
	case matchesKey(s, keys.Cancel):
		m.searching = false
		m.query = ""

	case matchesKey(s, keys.Accept):
		m.searching = false
	case matchesKey(s, keys.Toggle):
		m.query += " "
	case matchesKey(s, keys.HalfUp):
		m.query = ""
	case matchesKey(s, keys.Backspace):
		if m.query == "" {
			m.searching = false
		}
		r := []rune(m.query)
		if len(r) > 0 {
			m.query = string(r[:len(r)-1])
		}
	default:
		m.query += typedText(k)
	}
	m.follow = true
	m.rebuild()
	return nil

}

func (m *model) openIssue(project, uid string) tea.Cmd {
	if uid == "" {
		m.notify(statusInfo, "No linked issue")
		return nil
	}
	return issueCmd(project, uid)
}
