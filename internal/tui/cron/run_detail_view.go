package cron

import (
	tea "charm.land/bubbletea/v2"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func (m *model) detailRun() (records.Run, bool) {
	if m.currentView() != viewRun {
		return records.Run{}, false
	}
	r, ok := m.resolveFrame(m.nav[len(m.nav)-1])
	if !ok {
		return records.Run{}, false
	}
	return m.data.Runs[r.run], true
}

func (m *model) openJobOfRun() tea.Cmd {
	r, ok := m.detailRun()
	if !ok {
		return nil
	}
	uid := records.Value(r.JobUID)
	for _, j := range m.data.Jobs {
		if j.UID == uid && j.DeletedAt == nil && effectiveProjectUID(m.data, j.ProjectUID) == r.ProjectKey {
			cmd := m.pushDetail(viewJob, effectiveProjectUID(m.data, j.ProjectUID), j.UID)
			if records.Value(r.DefinitionEventUID) != "" && j.DefinitionEventUID != records.Value(r.DefinitionEventUID) {
				m.notify(statusInfo, "job definition changed since this run")
			}
			return cmd
		}
	}
	m.notify(statusInfo, "job "+"from this run"+" no longer exists")
	return nil
}
