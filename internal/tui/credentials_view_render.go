package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/helplayout"
)

const credentialCardRows = 5

func renderCredentials(m Model) string {
	state := m.credentials
	body := []string{
		titleStyle.Render("kata / credentials"),
		subtleStyle.Render(credentialsSummary(state)),
		"",
	}
	if state.capabilityPending {
		body = append(body, subtleStyle.Render("loading credential inventory..."))
	} else if !state.available && state.err != nil {
		body = append(body, errorStyle.Render(
			"failed to determine credential audit access: "+sanitizeForLine(state.err.Error())))
	} else if !state.available {
		body = append(body,
			errorStyle.Render("credential audit unavailable for this connection"),
			"",
			subtleStyle.Render("the active principal does not have token audit read capability"),
		)
	} else if state.loading && len(state.tokens) == 0 {
		body = append(body, subtleStyle.Render("loading credential inventory..."))
	} else if state.err != nil && len(state.tokens) == 0 {
		body = append(body, errorStyle.Render("failed to load credentials: "+sanitizeForLine(state.err.Error())))
	} else if len(state.tokens) == 0 {
		body = append(body, subtleStyle.Render("no credentials"))
	} else {
		body = append(body, renderVisibleCredentialCards(m)...)
		if state.err != nil {
			body = append(body, errorStyle.Render("refresh failed: "+sanitizeForLine(state.err.Error())))
		}
	}
	body = append(body, "", renderFooterHelpTable(
		modalFirstHelpRows(m.modal, credentialsHelpRows()), m.width))
	return strings.Join(body, "\n")
}

func credentialsSummary(state credentialAuditState) string {
	if state.observedAt.IsZero() {
		return fmt.Sprintf("%d credentials", len(state.tokens))
	}
	return fmt.Sprintf("%d credentials · observed at %s",
		len(state.tokens), formatCredentialTime(state.observedAt))
}

func renderVisibleCredentialCards(m Model) []string {
	state := m.credentials
	availableRows := len(state.tokens)
	if m.height > 0 {
		availableRows = max((m.height-6)/credentialCardRows, 1)
	}
	start, end := windowBounds(len(state.tokens), state.cursor, availableRows)
	out := make([]string, 0, (end-start)*credentialCardRows)
	for i := start; i < end; i++ {
		out = append(out, renderCredentialCard(state.tokens[i], i == state.cursor, m.width)...)
	}
	return out
}

func renderCredentialCard(token TokenInfo, selected bool, width int) []string {
	marker := "  "
	style := lipgloss.NewStyle()
	if selected {
		marker = "▶ "
		style = selectedStyle
	}
	name := "—"
	if token.Name != nil && *token.Name != "" {
		name = sanitizeForLine(*token.Name)
	}
	actor := sanitizeForLine(token.Actor)
	scope, project, root := "global", "—", "—"
	if token.Scope != nil {
		scope = sanitizeForLine(token.Scope.Kind)
		project = sanitizeForLine(token.Scope.ProjectUID)
		root = sanitizeForLine(token.Scope.RootIssueUID)
	}
	lines := []string{
		fmt.Sprintf("%sstate %s  ID %d  name %s  actor %s", marker,
			sanitizeForLine(token.State), token.ID, name, actor),
		fmt.Sprintf("  scope %s  project %s  root %s", scope, project, root),
		fmt.Sprintf("  created %s  expires %s", formatCredentialTime(token.CreatedAt), formatOptionalCredentialTime(token.ExpiresAt)),
		fmt.Sprintf("  last observed use %s  revoked %s", formatOptionalCredentialTime(token.LastUsedAt), formatOptionalCredentialTime(token.RevokedAt)),
		"",
	}
	for i := 0; i < len(lines)-1; i++ {
		lines[i] = style.Render(ansi.Truncate(lines[i], max(width, 1), "…"))
	}
	return lines
}

func formatCredentialTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2006-01-02 15:04:05Z")
}

func formatOptionalCredentialTime(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return formatCredentialTime(*value)
}

func credentialsHelpRows() [][]helplayout.HelpItem {
	return [][]helplayout.HelpItem{{
		{Key: "↑↓", Description: "move"},
		{Key: "r", Description: "refresh"},
		{Key: "esc", Description: "back"},
		{Key: "D", Description: "daemons"},
		{Key: "F", Description: "federation"},
		{Key: "P", Description: "projects"},
		{Key: "?", Description: "help"},
		{Key: "q", Description: "quit"},
	}}
}
