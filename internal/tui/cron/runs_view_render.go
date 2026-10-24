package cron

import (
	"time"

	"charm.land/lipgloss/v2"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func (m *model) runsColumns() []column {
	return executionColumns("detail", "detail", "started")
}

// RunsPage builds the visible page's cells once, for both sizing and drawing.
func (m *model) runsPage() [][]cell {
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	rows := make([][]cell, 0, end-start)
	for _, r := range m.rows[start:end] {
		rows = append(rows, m.runCells(r))
	}
	return rows
}

func (m *model) runsWidthsFor(rows [][]cell) []int {
	return listWidths(m.runsColumns(), rows, m.listWidth())
}

func (m *model) runCells(r row) []cell {
	rr := m.data.Runs[r.run]
	v := m.runViews[r.identity()]
	if v.Run.UID != rr.UID {
		v = m.runView(rr)
		v.Issue = m.runIssueRef(rr)
	}
	label, role := runLabel(rr)
	detail, detailRole := runDetailCell(rr, m.now())
	started := "—"
	if !v.When.IsZero() {
		started = ago(m.now(), v.When)
	}
	project := m.projectName(rr.ProjectKey)
	text := detailMetaStyle
	issue := m.listIssueRef(project, v.Issue)
	cells := []cell{{Text: issue, Style: text}, {Text: label, Style: role}, {Text: m.listName(project, v.Job, issue), Style: text}, {Text: detail, Style: detailRole}, {Text: started, Style: text}}
	return dimPlaceholders(cells)
}

func (m *model) renderRuns() bodyLines {
	page := m.runsPage()
	widths := m.runsWidthsFor(page)
	body := bodyLines{headerRow(m.runsColumns(), widths)}
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	for i := start; i < end; i++ {
		r := m.rows[i]
		line := parentRow(page[i-start], widths, i == m.cursor, m.frame, i, runExpandable(m.data.Runs[r.run]), m.expanded[r.uid])
		line.Hit = &rowHit{Kind: "run", Key: r.identity(), Index: i, X0: 0, X1: m.listWidth()}
		body = append(body, line)

	}
	if len(m.rows) == 0 {
		body = append(body, bodyLine{Text: subtleStyle.Render("  none")})
	}
	return body
}

func runNote(r records.Run, _ time.Time) (string, lipgloss.Style) {
	return clean(r.Summary.Message), detailMetaStyle
}
func runDetailCell(r records.Run, now time.Time) (string, lipgloss.Style) { return runNote(r, now) }
