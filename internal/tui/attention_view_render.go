package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
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
	budget := attentionBodyBudget(m)
	if s.err != nil {
		body = append(body, errorStyle.Render(truncate("failed to load attention requests: "+sanitizeForLine(s.err.Error()), width)))
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

func attentionBodyBudget(m Model) int {
	header := 3
	if m.attention.err != nil {
		header++
	}
	footer := renderFooterHelpTable(modalFirstHelpRows(m.modal, attentionHelpRows(m)), max(m.width, 1))
	return max(m.height-header-1-len(strings.Split(footer, "\n")), 0)
}

func attentionMessageViewport(s attentionState, width, budget int) ([]string, int) {
	row := s.rows[min(s.cursor, len(s.rows)-1)]
	var context []string
	if row.broadcast {
		context = append(context, "broadcast")
	}
	if row.kind != "" {
		context = append(context, sanitizeForLine(row.kind))
	}
	if row.re != "" {
		context = append(context, "re "+sanitizeForLine(row.re))
	}
	text := row.message
	if len(context) > 0 {
		text = strings.Join(context, " · ") + "\n" + text
	}
	message := wrapBody(strings.ReplaceAll(sanitizeForDisplay(text), "\t", "    "), width)
	panel := min(len(message), max(3, budget/3))
	return message, min(panel, max(budget-3, 0))
}

func renderAttentionRows(s attentionState, width, budget int) []string {
	if budget <= 0 {
		return nil
	}
	message, panel := attentionMessageViewport(s, width, budget)
	issueWidth := 8
	if s.allProjects {
		issueWidth = min(28, width/4)
	}
	navWidth := listColumnWidths(width, false).nav
	ownerWidth := min(24, width/5)
	attentionWidth := 11
	titleWidth := max(width-navWidth-issueWidth-ownerWidth-attentionWidth-6, 1)
	widths := []int{navWidth, issueWidth + 2, attentionWidth + 2, titleWidth + 2, ownerWidth}
	start, end := windowBounds(len(s.rows), s.cursor, max(budget-panel-2, 1))
	rows := make([][]string, 0, end-start)
	for i := start; i < end; i++ {
		r := s.rows[i]
		ref := "#" + r.issue.ShortID
		if s.allProjects {
			ref = r.issue.QualifiedID
		}
		owner := sanitizeForLine(r.from)
		if r.teammate != "" {
			owner += " / " + sanitizeForLine(r.teammate)
		}
		attention := r.workAttention
		if attention == "" {
			attention = "—"
		}
		nav := ""
		if i == s.cursor {
			nav = "▶"
		}
		rows = append(rows, []string{nav, truncate(sanitizeForLine(ref), issueWidth), truncate(sanitizeForLine(attention), attentionWidth), truncate(sanitizeForLine(r.issue.Title), titleWidth), truncate(owner, ownerWidth)})
	}
	// Match the issue list's table cells, row backgrounds, and plain ref,
	// title, and owner styling. Only attention has a semantic foreground.
	t := table.New().
		Border(lipgloss.HiddenBorder()).
		BorderTop(false).BorderBottom(false).BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderRow(false).BorderHeader(false).
		Width(width).Wrap(false).
		Headers("", "#", "attention", "title", "owner").Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			cell := lipgloss.NewStyle().Width(widths[col])
			if col > 0 && col < 4 {
				cell = cell.PaddingRight(2)
			}
			if row == table.HeaderRow {
				return cell.Inherit(tableHeaderStyle)
			}
			rowStyle := normalRowStyle
			index := start + row
			if index == s.cursor {
				rowStyle = cursorRowStyle
			} else if index%2 == 1 {
				rowStyle = altRowStyle
			}
			if col == 2 {
				switch s.rows[index].workAttention {
				case "stuck":
					cell = cell.Inherit(errorStyle)
				case "needs-human":
					cell = cell.Inherit(chipActive)
				default:
					cell = cell.Inherit(subtleStyle)
				}
			}
			return cell.Inherit(rowStyle)
		})
	out := strings.Split(t.Render(), "\n")

	if len(message) > 0 && panel > 0 {
		start := min(s.messageScroll, max(len(message)-panel, 0))
		end := min(start+panel, len(message))
		position := ""
		if len(message) > panel {
			position = subtleStyle.Render(fmt.Sprintf("message %d–%d/%d", start+1, end, len(message)))
		}
		out = append(out, position)
		out = append(out, message[start:end]...)
	}
	return out
}

func attentionHelpRows(_ Model) [][]helplayout.HelpItem {
	return [][]helplayout.HelpItem{{{Key: "↑↓", Description: "move"}, {Key: "pgup/pgdn", Description: "message"}, {Key: "↵", Description: "open"}, {Key: "c", Description: "clear"}, {Key: "r", Description: "refresh"}, {Key: "esc", Description: "back"}, {Key: "D", Description: "daemons"}, {Key: "F", Description: "federation"}, {Key: "C", Description: "credentials"}, {Key: "?", Description: "help"}, {Key: "q", Description: "quit"}}}
}
