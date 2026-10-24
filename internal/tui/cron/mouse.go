package cron

import tea "charm.land/bubbletea/v2"

func (m *model) mouse(msg tea.MouseMsg) tea.Cmd {
	_, click := msg.(tea.MouseClickMsg)
	_, wheel := msg.(tea.MouseWheelMsg)
	v := msg.Mouse()
	if m.searching || m.editor != nil || len(m.projectChoices) > 0 || !click && !wheel {
		return nil
	}
	if m.conflict != nil || len(m.confirm) > 0 {
		if v.Button == tea.MouseWheelUp {
			m.modalScroll = max(0, m.modalScroll-3)
		}
		if v.Button == tea.MouseWheelDown {
			m.modalScroll += 3
			if len(m.confirm) > 0 {
				m.modalScroll = min(m.modalScroll, len(m.confirm)-1)
			}
		}
		return nil
	}
	if v.Button == tea.MouseWheelUp {
		m.move(-3)
		return nil
	}
	if v.Button == tea.MouseWheelDown {
		m.move(3)
		return nil
	}
	if v.Button != tea.MouseLeft {
		return nil
	}
	hit, ok := m.hits.At(v.X, v.Y)
	if !ok {
		return nil
	}
	if m.currentView() != viewList {
		items := m.detailItems()
		for _, item := range items {
			if item.Key == hit.Key {
				same := m.detailKey == hit.Key
				m.detailKey, m.detailOrdinal = hit.Key, item.Index
				m.follow = true
				if same && m.currentView() == viewWorkflow {
					return m.workflowDetailKey(tea.KeyPressMsg{Code: 'l', Text: "l"})
				}
				return nil
			}
		}
		return nil
	}
	if hit.Index == m.cursor {
		return m.open()
	}
	m.cursor = hit.Index
	m.follow = true

	return nil
}
