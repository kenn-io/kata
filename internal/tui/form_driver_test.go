package tui

import tea "charm.land/bubbletea/v2"

// formDriver feeds keystrokes through the model shared by form tests.
type formDriver struct {
	m Model
}

func (h *formDriver) key(msg tea.KeyPressMsg) tea.Cmd {
	var cmd tea.Cmd
	h.m, cmd = stepModel(h.m, msg)
	return cmd
}
