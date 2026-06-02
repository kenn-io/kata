package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type federationMode int

const (
	federationModeList federationMode = iota
	federationModeDetail
)

func (m Model) transitionToFederation() (Model, tea.Cmd) {
	m.prevView = m.view
	m.view = viewFederation
	m.federationMode = federationModeList
	m.federationLoading = true
	m.federationErr = nil
	m.federationGen++
	return m, m.fetchFederationStatus()
}

func (m Model) fetchFederationStatus() tea.Cmd {
	api := m.api
	connGen := m.connGen
	gen := m.federationGen
	return func() tea.Msg {
		if api == nil {
			return federationLoadedMsg{connGen: connGen, gen: gen, err: errors.New("daemon client unavailable")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		instance, err := api.GetInstance(ctx)
		if err != nil {
			return federationLoadedMsg{connGen: connGen, gen: gen, err: err}
		}
		status, err := api.FederationStatus(ctx)
		return federationLoadedMsg{
			connGen:  connGen,
			gen:      gen,
			instance: instance,
			status:   status,
			err:      err,
		}
	}
}

func (m Model) handleFederationLoaded(msg federationLoadedMsg) Model {
	if m.staleConnMsg(msg.connGen) || msg.gen != m.federationGen {
		return m
	}
	m.federationLoading = false
	m.federationErr = msg.err
	if msg.err != nil {
		return m
	}
	m.federationInstance = msg.instance
	m.federationStatuses = msg.status.Statuses
	m.federationCursor = clampFederationCursor(m.federationCursor, federationSpokeStatuses(m.federationStatuses))
	return m
}

func (m Model) routeFederationViewKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	rows := federationSpokeStatuses(m.federationStatuses)
	if m.federationMode == federationModeDetail {
		switch msg.String() {
		case "esc", "backspace":
			m.federationMode = federationModeList
			return m, nil
		}
	}
	if next, ok := m.cursorMoveFederation(msg, rows); ok {
		return next, nil
	}
	switch msg.String() {
	case "esc":
		return m.escFromFederationView()
	case "r":
		m.federationLoading = true
		m.federationErr = nil
		m.federationGen++
		return m, m.fetchFederationStatus()
	case "enter":
		if m.federationCursor < 0 || m.federationCursor >= len(rows) {
			return m, nil
		}
		m.federationMode = federationModeDetail
		return m, nil
	}
	return m, nil
}

func (m Model) cursorMoveFederation(msg tea.KeyMsg, rows []FederationProjectStatus) (Model, bool) {
	switch msg.String() {
	case "j", "down":
		if m.federationCursor < len(rows)-1 {
			m.federationCursor++
		}
		return m, true
	case "k", "up":
		if m.federationCursor > 0 {
			m.federationCursor--
		}
		return m, true
	case "g", "home":
		m.federationCursor = 0
		return m, true
	case "G", "end":
		m.federationCursor = len(rows) - 1
		if m.federationCursor < 0 {
			m.federationCursor = 0
		}
		return m, true
	}
	return m, false
}

func (m Model) escFromFederationView() (Model, tea.Cmd) {
	if m.prevView == viewFederation {
		m.view = viewList
		return m, nil
	}
	m.view = m.prevView
	if m.view == viewHelp {
		m.view = viewList
	}
	return m, nil
}

func federationSpokeStatuses(statuses []FederationProjectStatus) []FederationProjectStatus {
	rows := make([]FederationProjectStatus, 0, len(statuses))
	for _, status := range statuses {
		if status.Role == "spoke" {
			rows = append(rows, status)
		}
	}
	return rows
}

func clampFederationCursor(cursor int, rows []FederationProjectStatus) int {
	if len(rows) == 0 || cursor < 0 {
		return 0
	}
	if cursor >= len(rows) {
		return len(rows) - 1
	}
	return cursor
}

func (m *Model) moveFederationCursor(delta int) {
	rows := federationSpokeStatuses(m.federationStatuses)
	if delta < 0 && m.federationCursor > 0 {
		m.federationCursor--
	}
	if delta > 0 && m.federationCursor < len(rows)-1 {
		m.federationCursor++
	}
}

func (m Model) mouseFederationClick(y int) (Model, tea.Cmd) {
	row := y - projectsFirstRowY
	if row < 0 {
		return m, nil
	}
	rows := federationSpokeStatuses(m.federationStatuses)
	if len(rows) == 0 {
		return m, nil
	}
	budget := len(rows)
	if m.height > 0 {
		budget = m.height - federationViewChromeRows
		if budget < 1 {
			budget = 1
		}
	}
	start, end := windowBounds(len(rows), m.federationCursor, budget)
	idx := start + row
	if idx < start || idx >= end || idx >= len(rows) {
		return m, nil
	}
	m.federationCursor = idx
	return m, nil
}
