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

// actorlessConnectionLabel explains read-only access without an account.
func actorlessConnectionLabel(kind string) string {
	switch kind {
	case "bootstrap":
		return "Bootstrap token"
	case "insecure_readonly":
		return "Anonymous (read-only)"
	case "trusted_proxy_absent":
		return "Proxy without account"
	default:
		return "No account"
	}
}

// activeConnectionStatus is empty when no connection state needs attention.
// Authentication checks apply only after capability discovery is required;
// transport failures also matter on older daemons without that discovery.
func (m Model) activeConnectionStatus() string {
	switch {
	case m.authCapabilitiesRequired && m.activeAuth.ExpiresAt != nil && !m.toastNow().Before(*m.activeAuth.ExpiresAt):
		return "Credential expired"
	case m.authCapabilitiesRequired && (m.activeAuthRejected || strings.Contains(strings.ToLower(m.activeAuthError), "authentication required")):
		return "Authentication required"
	case m.authCapabilitiesRequired && m.activeAuthError != "":
		return "Connection unavailable"
	case m.authCapabilitiesRequired && !m.authCapabilitiesReady:
		return "Connecting"
	case m.sseStatus == sseReconnecting:
		return "Reconnecting"
	case m.sseStatus == sseDisconnected:
		return "Disconnected"
	case m.authCapabilitiesRequired && (m.activeAuth.Kind == "insecure_readonly" || m.activeAuth.Kind == "bootstrap" || m.activeAuth.Kind == "trusted_proxy_absent" || !m.mutationAllowed(true)):
		return "Read-only"
	}
	return ""
}

func (m Model) connectionHeaderRows() int {
	if m.width > 0 && m.height > 1 && m.activeConnectionStatus() != "" {
		return 1
	}
	return 0
}

func (m Model) contentHeight() int {
	return max(m.height-m.connectionHeaderRows(), 0)
}

// Keep navigation pinned above the attention row. Renderers reserve its height
// before drawing their bodies, so footers and viewport geometry stay aligned.
func (m Model) withActiveConnectionHeader(body string) string {
	if m.connectionHeaderRows() == 0 {
		return body
	}
	status := m.activeConnectionStatus()
	account := "—"
	if status == "Read-only" {
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
	chrome := m.chrome()
	left, showHub, right := missingConnectionHeaderContext(first, chrome)
	if !showHub {
		hub = ""
	}
	summary := m.connectionSummaryRow(status, hub, account, left, right)
	summary = lipgloss.NewStyle().Background(lipgloss.Color("#fff3bb")).Foreground(lipgloss.Color("#493800")).Render(summary)
	if more {
		return first + "\n" + summary + "\n" + rest
	}
	return first + "\n" + summary
}

// missingConnectionHeaderContext returns only title-bar context not already
// visible on the adjacent navigation row. The Hub field carries daemon
// context when that label was truncated from the row.
func missingConnectionHeaderContext(header string, chrome viewChrome) (left string, showHub bool, right string) {
	visible := ansi.Strip(header)
	left = titleBarLeft(chrome.scope)
	if strings.Contains(visible, left) {
		left = ""
	}
	daemon := sanitizeForLine(chrome.daemon)
	showHub = daemon == "" || !strings.Contains(visible, "Daemon: "+daemon)
	rightContext := []string{}
	if !strings.Contains(visible, "kata カタ") && !strings.Contains(visible, "[kata] カタ") {
		rightContext = append(rightContext, "kata カタ")
	}
	version := sanitizeForLine(chrome.version)
	if version != "" && !strings.Contains(visible, version) {
		rightContext = append(rightContext, version)
	}
	right = strings.Join(rightContext, " · ")
	return left, showHub, right
}

// connectionSummaryRow fits status, missing project context, hub/account,
// then any remaining brand/version context into one row.
func (m Model) connectionSummaryRow(status, hub, account, left, right string) string {
	width := m.width
	tail := ""
	if left != "" {
		tail = ansi.Truncate(" · "+left, max(width-ansi.StringWidth(status), 0), "…")
	}
	hubLabel := ""
	if hub != "" {
		hubLabel = " · Hub: "
	}
	accountLabel := " · Account: "
	labelBudget := max(width-ansi.StringWidth(status+hubLabel+accountLabel+tail), 0)
	hubWidth := 0
	accountWidth := labelBudget
	if hub != "" {
		accountWidth = min(ansi.StringWidth(account), labelBudget-labelBudget/2)
		hubWidth = min(ansi.StringWidth(hub), max(labelBudget-accountWidth, 0))
		accountWidth = min(ansi.StringWidth(account), max(labelBudget-hubWidth, 0))
	}
	summary := status
	if hub != "" {
		summary += hubLabel + ansi.Truncate(hub, hubWidth, "…")
	}
	summary += accountLabel + ansi.Truncate(account, accountWidth, "…")
	row := ansi.Truncate(summary, max(width-ansi.StringWidth(tail), 0), "…") + tail
	if room := width - ansi.StringWidth(row) - 1; right != "" && room > 0 {
		right = ansi.Truncate(right, room, "…")
		row += strings.Repeat(" ", width-ansi.StringWidth(row)-ansi.StringWidth(right)) + right
	}
	return row
}
