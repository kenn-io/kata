package cron

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

// Execution lists follow Kata's issue list: #, then status, then the record's
// own columns, with the time column last. Fixed columns fit their content and
// the flex column takes the rest. On a narrow pane the flex column gives way
// first, then model, then the secondary columns, then job, then the time; #
// and status never shrink.
func executionColumns(flex, detailTitle, timeTitle string, extra ...column) []column {
	cols := []column{{Title: "#"}, {Title: "status"}}
	cols = append(cols, column{Title: "job", Width: 40, Shrink: 3}, column{Title: detailTitle, Width: 24, Shrink: 2})
	cols = append(cols, extra...)
	cols = append(cols, column{Title: timeTitle, Shrink: 4})
	for i := range cols {
		if cols[i].Title == flex {
			cols[i].Flex, cols[i].Width = true, 0
		}
	}
	return cols
}

// JobsColumns adds the workflow and next columns while the job column would
// still keep 30 cells.
func (m *model) jobsColumns() []column {
	var extra []column
	for _, title := range []string{"workflow"} {
		used := 0
		for _, c := range executionColumns("job", "schedule", "updated", extra...) {
			used += max(c.Width, 9) + 1
		}
		if m.listWidth()-2-used-15 >= 30 {
			extra = append(extra, column{Title: title, Width: 14, Shrink: 2})
		}
	}
	return executionColumns("job", "schedule", "updated", extra...)
}

// JobsPage builds the visible page's cells once, for both sizing and drawing.
func (m *model) jobsPage() [][]cell {
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	rows := make([][]cell, 0, end-start)
	for _, r := range m.rows[start:end] {
		rows = append(rows, m.jobCells(m.indexedJob(r)))
	}
	return rows
}

func (m *model) jobsLayoutFor(rows [][]cell) ([]int, int) {
	r, ok := m.selected()
	if geometry, split := m.splitGeometry(); split && ok && r.kind == "job" {
		return listWidths(m.jobsColumns(), rows, geometry.ListInnerW), geometry.DetailInnerW
	}
	return listWidths(m.jobsColumns(), rows, m.width), 0
}

func (m *model) jobCells(v *jobView) []cell {
	if v == nil {
		return []cell{}
	}
	name := v.entry.Name
	if strings.TrimSpace(name) == "" {
		name = "unnamed job"
	}
	// Like Kata's context rows, inactive definitions dim their whole row.
	row := detailMetaStyle
	role := detailMetaStyle
	switch {
	case !v.def.Enabled:
		name += " (paused)"
		row, role = subtleStyle, subtleStyle
	case v.decodeErr != nil:
		name += " (invalid)"
		role = attentionStyle
	}
	last := "unloaded"
	lastRole := subtleStyle
	updated := v.entry.UpdatedAt
	if v.entry.CreatedAt.After(updated) {
		updated = v.entry.CreatedAt
	}
	if len(v.runIndexes) > 0 {
		r := m.data.Runs[v.runIndexes[0]]
		last, lastRole = runLabel(r)
		updated = latestActivity(updated, r)
	}
	project := v.entry.Project
	if project == "" {
		project = m.projectName(v.key.projectUID)
	}
	issue := m.listIssueRef(project, v.issue)
	cells := make([]cell, 0, len(m.jobsColumns()))
	for _, col := range m.jobsColumns() {
		c := cell{Style: row}
		switch col.Title {
		case "#":
			c.Text = issue
		case "status":
			c = cell{Text: last, Style: lastRole}
		case "job":
			c = cell{Text: m.listName(project, name, issue), Style: role}
		case "schedule":
			c.Text = v.schedule
		case "workflow":
			c.Text = v.workflowName
			if c.Text == "" {
				c.Text = "—"
			}
		case "updated":
			c.Text = orDash(ago(m.now(), updated))
		}
		cells = append(cells, c)
	}
	return dimPlaceholders(cells)
}

// dimPlaceholders applies Kata's list rule for empty values: a — cell is
// subtle while real values keep their row's role.
func dimPlaceholders(cells []cell) []cell {
	for i := range cells {
		if cells[i].Text == "—" {
			cells[i].Style = subtleStyle
		}
	}
	return cells
}

// LatestActivity is the later of since and the run's start or end.
func latestActivity(since time.Time, r records.Run) time.Time {
	for _, at := range []*time.Time{r.StartedAt, r.EndedAt} {
		if at != nil && at.After(since) {
			since = *at
		}
	}
	return since
}

func orDash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}

func (m *model) renderJobs() bodyLines {
	page := m.jobsPage()
	widths, inspector := m.jobsLayoutFor(page)
	table := min(max(0, m.width-2), tableWidth(widths))
	body := bodyLines{headerRow(m.jobsColumns(), widths)}
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	for i := start; i < end; i++ {
		r := m.rows[i]
		line := tableRow(page[i-start], widths, i == m.cursor, m.frame, i)
		line.Hit = &rowHit{Kind: "job", Key: r.identity(), Index: i, X0: 0, X1: table + 2}
		body = append(body, line)
	}
	if len(m.rows) == 0 {
		message := "no jobs yet — press n to add one"
		if m.query != "" {
			message = "no jobs match " + clean(m.query)
		}
		body = append(body, bodyLine{Text: "  " + subtleStyle.Render(message)})
	}
	if width := inspector; width > 0 {
		if _, split := m.splitGeometry(); split {
			return body
		}
		if r, ok := m.selected(); ok {
			body = beside(body, m.renderJobInspector(m.indexedJob(r), width), table+2, 3)
		}
	}
	return body
}

func (m *model) jobsExtras() []string {
	var extras []string
	if m.query != "" {
		extras = append(extras, fmt.Sprintf("%d of %d match", m.jobMatches, len(m.jobs.byKey)))
	}
	return extras
}

func jobStateCells(v *jobView) []cell {
	cells := []cell{{Text: "enabled", Style: detailMetaStyle}}
	if !v.def.Enabled {
		cells = []cell{{Text: "paused", Style: attentionStyle}}
	}
	if v.shape == "recurring" {
		cells = append(cells, cell{Text: " · native", Style: detailMetaStyle})
	} else if v.decodeErr != nil {
		cells = append(cells, cell{Text: " · invalid", Style: attentionStyle})
	}
	return cells
}

func (m *model) renderJobInspector(v *jobView, width int) bodyLines {
	if v == nil {
		return nil
	}
	name := v.entry.Name
	if name == "" {
		name = "unnamed job"
	}
	body := bodyLines{inspectorLine(name, width, m.frame, titleStyle), {}}
	field := func(label string, cells ...cell) {
		body = append(body, inspectorKVCells(label, cells, width, m.frame, 9)...)
	}
	plain := func(label, text string) { field(label, cell{Text: text, Style: detailMetaStyle}) }
	if m.showProjects() {
		plain("project", m.projectName(v.key.projectUID))
	} else if v.key.projectUID == "" {
		field("project", cell{Text: "project unpinned", Style: attentionStyle})
	}
	field("state", jobStateCells(v)...)
	plain("schedule", v.schedule)
	if v.shape == "workflow" {
		name := v.workflowName
		if name == "" {
			name = "workflow unavailable"
		}
		plain("workflow", name)
		plain("input", v.input)
	}
	field("tags", cell{Text: labelText(v.tags, max(0, width-10)), Style: chipStyle})
	parent := defs.RunParent(v.def)
	if v.shape == "recurring" && v.def.Issue != nil {
		parent = v.def.Issue.ParentUID
	}
	plain("parent", m.knownIssueRef(v.key.projectUID, parent))
	body = append(body, bodyLine{})
	if len(v.runIndexes) == 0 {
		return append(body, bodyLine{Text: subtleStyle.Render("no loaded run observations")})
	}
	r := m.data.Runs[v.runIndexes[0]]
	label, role := runLabel(r)
	when := ""
	if r.StartedAt != nil {
		when = " · " + ago(m.now(), *r.StartedAt)
	}
	body = append(body, bodyLine{Text: detailSectionHeaderStyle.Render("Last run")}, bodyLine{Text: ansi.Truncate(role.Render(clean(label))+detailMetaStyle.Render(clean(when)), width, "")})
	note, noteRole := runNote(r, m.now())
	body = append(body, jobInspectorProse(note, noteRole, width, 3)...)
	return body
}

func jobInspectorProse(text string, role lipgloss.Style, width, limit int) bodyLines {
	var body bodyLines
	for _, line := range wrapMax(text, width, limit) {
		body = append(body, bodyLine{Text: role.Render(line)})
	}
	return body
}
