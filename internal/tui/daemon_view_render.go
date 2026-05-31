package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

const daemonsViewChromeRows = 8

func renderDaemons(m Model) string {
	rows := daemonRows(m.daemonTargets, m.activeDaemon)
	cursor := m.daemonCursor
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	rowBudget := len(rows)
	if m.height > 0 {
		rowBudget = m.height - daemonsViewChromeRows
		if rowBudget < 1 {
			rowBudget = 1
		}
	}
	visible := clipDaemonRows(rows, cursor, rowBudget)
	body := []string{
		titleStyle.Render("kata / daemons"),
		subtleStyle.Render(fmt.Sprintf("%d daemons", len(rows))),
		"",
		renderDaemonHeader(m.width),
	}
	for _, vr := range visible {
		body = append(body, renderDaemonRow(vr.row, vr.index == cursor, m.width))
	}
	body = append(body, "")
	if cursor >= 0 && cursor < len(rows) {
		body = append(body, subtleStyle.Render(daemonFooter(rows[cursor], m.width)))
	}
	body = append(body, "")
	body = append(body, subtleStyle.Render(
		"[↑/↓ k/j] move  [enter] switch  [esc] back  [q] quit  [?] help"))
	return strings.Join(body, "\n")
}

type daemonVisibleRow struct {
	row   daemonRow
	index int
}

func clipDaemonRows(rows []daemonRow, cursor, budget int) []daemonVisibleRow {
	if budget <= 0 || len(rows) == 0 {
		return nil
	}
	if len(rows) <= budget {
		out := make([]daemonVisibleRow, 0, len(rows))
		for i, row := range rows {
			out = append(out, daemonVisibleRow{row: row, index: i})
		}
		return out
	}
	start, end := windowBounds(len(rows), cursor, budget)
	out := make([]daemonVisibleRow, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, daemonVisibleRow{row: rows[i], index: i})
	}
	return out
}

func renderDaemonHeader(width int) string {
	return daemonRowLayout("Daemon", "Endpoint", "Auth", "State", width, false)
}

func renderDaemonRow(row daemonRow, highlight bool, width int) string {
	state := ""
	if row.current {
		state = "current"
	}
	return daemonRowLayout(
		sanitizeForLine(daemonName(row.target)),
		sanitizeForLine(daemonEndpoint(row.target)),
		daemonAuth(row.target),
		state,
		width,
		highlight,
	)
}

func daemonRowLayout(name, endpoint, auth, state string, width int, highlight bool) string {
	const (
		authW  = 8
		stateW = 10
		gap    = 2
	)
	nameW := 22
	endpointW := width - nameW - authW - stateW - 3*gap - 2
	if endpointW < 12 {
		endpointW = 12
	}
	cursor := "  "
	if highlight {
		cursor = "▶ "
	}
	line := cursor + padToWidth(name, nameW) +
		strings.Repeat(" ", gap) + padToWidth(endpoint, endpointW) +
		strings.Repeat(" ", gap) + padToWidth(auth, authW) +
		strings.Repeat(" ", gap) + padToWidth(state, stateW)
	if highlight {
		line = lipgloss.NewStyle().Bold(true).Render(line)
	}
	return line
}

func daemonName(target daemonTarget) string {
	if target.Name != "" {
		return target.Name
	}
	return daemonTargetDisplay(target)
}

func daemonEndpoint(target daemonTarget) string {
	if target.Local {
		return "local"
	}
	return daemonTargetDisplay(daemonTarget{URL: target.URL})
}

func daemonAuth(target daemonTarget) string {
	if target.Token != "" || target.TokenEnv != "" {
		return "token"
	}
	return "no token"
}

func daemonFooter(row daemonRow, width int) string {
	text := sanitizeForLine(daemonName(row.target)) + " · " + sanitizeForLine(daemonEndpoint(row.target))
	if row.target.AllowInsecure {
		text += " · allow_insecure"
	}
	return truncate(text, width)
}
