package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type activeCredentialExpiredMsg struct {
	connGen   uint64
	expiresAt time.Time
}

func (m *Model) activeCredentialExpiryCmd() tea.Cmd {
	if m.activeAuth.ExpiresAt == nil {
		return nil
	}
	expiry := *m.activeAuth.ExpiresAt
	delay := expiry.Sub(m.toastNow())
	if delay <= 0 {
		m.authCapabilitiesReady = false
		return nil
	}
	gen := m.connGen
	return tea.Tick(delay, func(time.Time) tea.Msg { return activeCredentialExpiredMsg{connGen: gen, expiresAt: expiry} })
}

// Reuse the existing header row. No body, mouse, or modal coordinates move.
func (m Model) withActiveConnectionHeader(body string) string {
	if !m.authCapabilitiesRequired || m.width <= 0 {
		return body
	}
	status := "Connected"
	switch {
	case m.activeAuth.ExpiresAt != nil && !m.toastNow().Before(*m.activeAuth.ExpiresAt):
		status = "Credential expired"
	case m.activeAuthRejected || strings.Contains(strings.ToLower(m.activeAuthError), "authentication required"):
		status = "Authentication required"
	case m.activeAuthError != "":
		status = "Connection unavailable"
	case !m.authCapabilitiesReady:
		status = "Connecting"
	case m.sseStatus == sseReconnecting:
		status = "Reconnecting"
	case m.sseStatus == sseDisconnected:
		status = "Disconnected"
	}
	account := "—"
	if m.authCapabilitiesReady && m.sseStatus == sseConnected {
		account = sanitizeForLine(m.activeAuth.Actor)
		if account == "" {
			account = "Local connection"
		}
	}
	hub := sanitizeForLine(daemonName(m.activeDaemon))
	if hub == "local" {
		hub = "Local daemon"
	}
	first, rest, more := strings.Cut(body, "\n")
	labelBudget := max(m.width-ansi.StringWidth(status+" · Hub:  · Account: "), 0)
	hubWidth := min(ansi.StringWidth(hub), labelBudget/2)
	accountWidth := max(labelBudget-hubWidth, 0)
	summary := status + " · Hub: " + ansi.Truncate(hub, hubWidth, "…") +
		" · Account: " + ansi.Truncate(account, accountWidth, "…")
	summary = ansi.Truncate(summary, m.width, "…")
	if available := m.width - ansi.StringWidth(summary) - 3; available > 0 {
		summary += " · " + ansi.Truncate(ansi.Strip(first), available, "…")
	}
	summary = lipgloss.NewStyle().Background(lipgloss.Color("#fff3bb")).Foreground(lipgloss.Color("#493800")).Render(summary)
	if more {
		return summary + "\n" + rest
	}
	return summary
}
