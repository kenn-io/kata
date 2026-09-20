package tui

import (
	"context"
	"errors"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
)

const credentialsRefreshInterval = 30 * time.Second

type credentialAuditState struct {
	available         bool
	capabilityPending bool
	loading           bool
	err               error
	tokens            []TokenInfo
	observedAt        time.Time
	cursor            int
	gen               uint64
	refreshScheduled  bool
}

type credentialsLoadedMsg struct {
	connGen    uint64
	gen        uint64
	tokens     []TokenInfo
	observedAt time.Time
	err        error
}

type credentialsRefreshTickMsg struct {
	connGen uint64
	gen     uint64
}

func (m Model) transitionToCredentials() (Model, tea.Cmd) {
	if m.view != viewCredentials {
		m.prevView = m.view
	}
	m.credentials.gen++
	m.credentials.capabilityPending = m.authCapabilitiesRequired && !m.authCapabilitiesReady
	m.credentials.available = m.authCapabilitiesReady && m.tokenAuditRead
	m.credentials.loading = m.credentials.capabilityPending || m.credentials.available
	m.credentials.err = nil
	m.credentials.tokens = nil
	m.credentials.observedAt = time.Time{}
	m.credentials.cursor = 0
	m.credentials.refreshScheduled = false
	m.view = viewCredentials
	if m.credentials.capabilityPending {
		return m, m.fetchAuthCapabilities()
	}
	if !m.credentials.available {
		return m, nil
	}
	return m, m.fetchCredentials(m.credentials.gen)
}

func (m Model) leaveCredentialsView() Model {
	if m.view == viewCredentials {
		m.credentials.gen++
		m.credentials.loading = false
		m.credentials.refreshScheduled = false
	}
	return m
}

func (m Model) prepareCredentialsGlobalExit() Model {
	if m.view != viewCredentials {
		return m
	}
	returnView := m.prevView
	m = m.leaveCredentialsView()
	if returnView == viewCredentials {
		returnView = viewList
	}
	m.view = returnView
	return m
}

func (m Model) routeCredentialsViewKey(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m = m.leaveCredentialsView()
		if m.prevView == viewCredentials {
			m.view = viewList
		} else {
			m.view = m.prevView
		}
		return m.resumeInboxReresolve()
	case "r":
		if !m.credentials.available {
			if m.credentials.err != nil {
				m.credentials.capabilityPending = true
				m.credentials.loading = true
				m.credentials.err = nil
				return m, m.fetchAuthCapabilities()
			}
			return m, nil
		}
		m.credentials.loading = true
		m.credentials.err = nil
		return m, m.fetchCredentials(m.credentials.gen)
	case "j", "down":
		if m.credentials.cursor < len(m.credentials.tokens)-1 {
			m.credentials.cursor++
		}
		return m, nil
	case "k", "up":
		if m.credentials.cursor > 0 {
			m.credentials.cursor--
		}
		return m, nil
	case "g", "home":
		m.credentials.cursor = 0
		return m, nil
	case "G", "end":
		m.credentials.cursor = max(len(m.credentials.tokens)-1, 0)
		return m, nil
	}
	return m, nil
}

func (m Model) fetchCredentials(gen uint64) tea.Cmd {
	apiClient, ok := m.api.(credentialAuditAPI)
	connGen := m.connGen
	if !ok || apiClient == nil {
		return func() tea.Msg {
			return credentialsLoadedMsg{
				connGen: connGen, gen: gen,
				err: errors.New("credential audit client unavailable"),
			}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tokens, observedAt, err := apiClient.ListTokens(ctx)
		return credentialsLoadedMsg{
			connGen: connGen, gen: gen, tokens: tokens,
			observedAt: observedAt, err: err,
		}
	}
}

func (m Model) handleCredentialsLoaded(msg credentialsLoadedMsg) (Model, tea.Cmd) {
	if m.view != viewCredentials || m.staleConnMsg(msg.connGen) || msg.gen != m.credentials.gen {
		return m, nil
	}
	m.credentials.loading = false
	m.credentials.err = msg.err
	if msg.err == nil {
		m.credentials.tokens = append([]TokenInfo(nil), msg.tokens...)
		sort.SliceStable(m.credentials.tokens, func(i, j int) bool {
			if !m.credentials.tokens[i].CreatedAt.Equal(m.credentials.tokens[j].CreatedAt) {
				return m.credentials.tokens[i].CreatedAt.After(m.credentials.tokens[j].CreatedAt)
			}
			return m.credentials.tokens[i].ID > m.credentials.tokens[j].ID
		})
		m.credentials.observedAt = msg.observedAt
		m.credentials.cursor = min(m.credentials.cursor, max(len(m.credentials.tokens)-1, 0))
	}
	if m.credentials.refreshScheduled {
		return m, nil
	}
	m.credentials.refreshScheduled = true
	return m, m.scheduleCredentialsRefresh(msg.gen)
}

func (m Model) scheduleCredentialsRefresh(gen uint64) tea.Cmd {
	connGen := m.connGen
	return tea.Tick(credentialsRefreshInterval, func(time.Time) tea.Msg {
		return credentialsRefreshTickMsg{connGen: connGen, gen: gen}
	})
}

func (m Model) handleCredentialsRefreshTick(msg credentialsRefreshTickMsg) (Model, tea.Cmd) {
	if m.view != viewCredentials || m.staleConnMsg(msg.connGen) ||
		msg.gen != m.credentials.gen || !m.credentials.available {
		return m, nil
	}
	m.credentials.refreshScheduled = false
	m.credentials.loading = true
	return m, m.fetchCredentials(msg.gen)
}
