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

// actorlessConnectionLabel describes a connection that carries no account
// from the daemon's authentication kind. Only owner-local requests are local.
func actorlessConnectionLabel(kind string) string {
	switch kind {
	case "none", "web_local":
		return "Local connection"
	case "static_token":
		return "Shared daemon token"
	case "bootstrap":
		return "Bootstrap token"
	case "insecure_readonly":
		return "Anonymous (read-only)"
	case "unauthenticated_private_network":
		return "Unauthenticated private network"
	case "trusted_proxy_absent":
		return "Proxy without account"
	default:
		return "No account"
	}
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
			account = actorlessConnectionLabel(m.activeAuth.Kind)
		}
	}
	hub := sanitizeForLine(daemonName(m.activeDaemon))
	if hub == "local" {
		hub = "Local daemon"
	}
	first, rest, more := strings.Cut(body, "\n")
	left, right := m.headerRowContent(first)
	summary := m.connectionSummaryRow(status, hub, account, left, right)
	summary = lipgloss.NewStyle().Background(lipgloss.Color("#fff3bb")).Foreground(lipgloss.Color("#493800")).Render(summary)
	if more {
		return summary + "\n" + rest
	}
	return summary
}

// headerRowContent splits the row the summary replaces into the label that
// must survive (the title bar's project scope, or another view's heading)
// and the title bar's right cluster, which is shown only when room remains.
func (m Model) headerRowContent(first string) (string, string) {
	chrome := m.chrome()
	if first == renderTitleBar(m.width, chrome.scope, chrome.version, chrome.daemon) {
		return titleBarLeft(chrome.scope), titleBarRight(chrome.version, chrome.daemon)
	}
	return strings.TrimSpace(ansi.Strip(first)), ""
}

// connectionSummaryRow fits one row in priority order: status, the replaced
// row's label, hub and account, then the right cluster.
func (m Model) connectionSummaryRow(status, hub, account, left, right string) string {
	width := m.width
	tail := ""
	if left != "" {
		tail = ansi.Truncate(" · "+left, max(width-ansi.StringWidth(status), 0), "…")
	}
	labelBudget := max(width-ansi.StringWidth(status+tail+" · Hub:  · Account: "), 0)
	accountWidth := min(ansi.StringWidth(account), labelBudget-labelBudget/2)
	hubWidth := min(ansi.StringWidth(hub), labelBudget-accountWidth)
	accountWidth = min(ansi.StringWidth(account), labelBudget-hubWidth)
	summary := status + " · Hub: " + ansi.Truncate(hub, hubWidth, "…") +
		" · Account: " + ansi.Truncate(account, accountWidth, "…")
	row := ansi.Truncate(summary, max(width-ansi.StringWidth(tail), 0), "…") + tail
	if room := width - ansi.StringWidth(row) - 1; right != "" && room > 0 {
		right = ansi.Truncate(right, room, "…")
		row += strings.Repeat(" ", width-ansi.StringWidth(row)-ansi.StringWidth(right)) + right
	}
	return row
}
