package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/helplayout"
)

func renderAttention(m Model) string {
	s := m.attention
	width := max(m.width, 1)
	scopeName := s.projectName
	if s.allProjects {
		scopeName = "all projects"
	}
	header := fmt.Sprintf("for %s · %s · %d requests", sanitizeForLine(s.recipient), sanitizeForLine(scopeName), len(s.rows))
	if s.skipped > 0 {
		header += fmt.Sprintf(" · %d skipped", s.skipped)
	}
	body := []string{titleStyle.Render("kata / attention"), subtleStyle.Render(truncate(header, width)), ""}
	footer := renderFooterHelpTable(modalFirstHelpRows(m.modal, attentionHelpRows(m)), width)
	budget := max(m.height-len(body)-1-len(strings.Split(footer, "\n")), 0)
	if s.err != nil {
		body = append(body, errorStyle.Render(truncate("failed to load attention requests: "+sanitizeForLine(s.err.Error()), width)))
		budget = max(budget-1, 0)
	}
	switch {
	case s.loading && len(s.rows) == 0:
		body = append(body, subtleStyle.Render("Loading attention requests…"))
	case s.err == nil && len(s.rows) == 0:
		text := "No requests for " + sanitizeForLine(s.recipient)
		if s.allProjects {
			text += " across all projects"
		}
		body = append(body, subtleStyle.Render(truncate(text, width)))
	case len(s.rows) > 0:
		body = append(body, renderAttentionRows(s, width, budget)...)
	}
	// Keep the help at the bottom without exceeding a short terminal's height.
	body = append(body, "", footer)
	if m.height > 0 && len(body) > m.height {
		body = body[:m.height]
	}
	return strings.Join(body, "\n")
}

func renderAttentionRows(s attentionState, width, budget int) []string {
	if budget <= 0 {
		return nil
	}
	row := s.rows[min(s.cursor, len(s.rows)-1)]
	message := wrapBody(sanitizeForDisplay(row.message), width)
	panel := min(len(message), max(3, budget/3))
	panel = min(panel, max(budget-3, 0))
	refWidth := 8
	if s.allProjects {
		refWidth = min(28, width/4)
	}
	fromWidth := min(24, width/5)
	workWidth := 11
	titleWidth := max(width-refWidth-fromWidth-workWidth-8, 1)
	columns := func(ref, title, from, work string) string {
		return padToWidth(truncate(ref, refWidth), refWidth) + "  " + padToWidth(truncate(title, titleWidth), titleWidth) + "  " + padToWidth(truncate(from, fromWidth), fromWidth) + "  " + truncate(work, workWidth)
	}
	out := []string{subtleStyle.Render("  " + columns("Ref", "Title", "From", "Work"))}
	start, end := windowBounds(len(s.rows), s.cursor, max(budget-panel-2, 1))
	for i := start; i < end; i++ {
		r := s.rows[i]
		ref := r.issue.ShortID
		if s.allProjects {
			ref = r.issue.QualifiedID
		}
		from := sanitizeForLine(r.from)
		if r.teammate != "" {
			from += " / " + sanitizeForLine(r.teammate)
		}
		text := columns(sanitizeForLine(ref), sanitizeForLine(r.issue.Title), from, r.workAttention)
		if i == s.cursor {
			out = append(out, selectedStyle.Render("▶ "+text))
		} else {
			out = append(out, "  "+text)
		}
	}
	if panel > 0 {
		out = append(out, "")
		if len(message) > panel {
			message = message[:panel]
			message[panel-1] = ansi.Truncate(message[panel-1], max(width-1, 0), "") + "…"
		}
		out = append(out, message...)
	}
	return out
}

func attentionHelpRows(_ Model) [][]helplayout.HelpItem {
	return [][]helplayout.HelpItem{{{Key: "↑↓", Description: "move"}, {Key: "↵", Description: "open"}, {Key: "r", Description: "refresh"}, {Key: "esc", Description: "back"}, {Key: "D", Description: "daemons"}, {Key: "F", Description: "federation"}, {Key: "C", Description: "credentials"}, {Key: "?", Description: "help"}, {Key: "q", Description: "quit"}}}
}
