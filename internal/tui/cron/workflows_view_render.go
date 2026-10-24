package cron

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// WorkflowColumns follow the execution lists: fixed columns fit their content
// and the workflow column takes the rest. Input gives way first on a narrow
// pane, then steps and the time; status never shrinks.
func (m *model) workflowColumns() []column {
	return []column{{Title: "status"}, {Title: "workflow", Flex: true}, {Title: "steps", Shrink: 2}, {Title: "input", Width: 46, Shrink: 1}, {Title: "updated", Shrink: 3}}
}

// WorkflowPage builds the visible page's cells once, for both sizing and
// drawing; a row with no indexed workflow has nil cells.
func (m *model) workflowPage() [][]cell {
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	rows := make([][]cell, 0, end-start)
	for _, r := range m.rows[start:end] {
		cells := []cell{}
		if v := m.wfIndex[workflowKey{r.projectUID, r.uid}]; v != nil {
			cells = m.workflowCells(v)
		}
		rows = append(rows, cells)
	}
	return rows
}

func (m *model) workflowLayoutFor(rows [][]cell) ([]int, int) {
	if geometry, split := m.splitGeometry(); split {
		return listWidths(m.workflowColumns(), rows, geometry.ListInnerW), geometry.DetailInnerW
	}
	return listWidths(m.workflowColumns(), rows, m.width), 0
}

func workflowDiagnosis(c workflowCheck) string {
	if c.DecodeErr != nil {
		return "does not parse: " + c.DecodeErr.Error()
	}
	if len(c.Unsupported) > 0 {
		return "unsupported workflow: " + c.Unsupported[0]
	}
	return ""
}

func (m *model) workflowCells(v *workflowView) []cell {
	w := m.data.Workflows[v.index]
	steps, input := "—", "—"
	if v.check.Definition != nil {
		steps = fmt.Sprint(len(v.check.Definition.Steps))
		if v.check.Definition.Input != "" {
			input = v.check.Definition.Input
		}
	}
	label := "idle"
	role := subtleStyle
	if v.active {
		label = "active"
		role = openStyle
	}
	updated := w.UpdatedAt
	if w.CreatedAt.After(updated) {
		updated = w.CreatedAt
	}
	if len(v.runs) > 0 {
		r := m.data.Runs[v.runs[0]]
		label, role = runLabel(r)
		updated = latestActivity(updated, r)
	}
	// Like Kata's context rows, a workflow no enabled job uses dims its row.
	row := subtleStyle
	if v.active {
		row = detailMetaStyle
	}
	cells := []cell{{Text: label, Style: role}, {Text: m.listName(w.Project, w.Name, "—"), Style: row}, {Text: steps, Style: row}, {Text: input, Style: row}, {Text: orDash(ago(m.now(), updated)), Style: row}}
	return dimPlaceholders(cells)
}

func (m *model) renderWorkflows() bodyLines {
	page := m.workflowPage()
	widths, inspector := m.workflowLayoutFor(page)
	table := tableWidth(widths)
	body := bodyLines{headerRow(m.workflowColumns(), widths)}
	start, end, _, _ := pager(len(m.rows), m.cursor, m.pageRows())
	for i := start; i < end; i++ {
		r := m.rows[i]
		w := m.data.Workflows[r.wf]
		v := m.wfIndex[workflowKey{r.projectUID, r.uid}]
		if v == nil {
			continue
		}
		problem := workflowDiagnosis(v.check)
		role := failStyle
		if problem == "" && !v.laneAvailable {
			problem = "project unavailable"
			role = attentionStyle
		}
		var line bodyLine
		if problem != "" {
			text := "! " + m.listName(w.Project, w.Name, "—") + ": " + problem
			text = fitCell(text, table)
			line = tableRow([]cell{{Text: text, Style: role}}, []int{table}, i == m.cursor, 0, i)
		} else {
			line = parentRow(page[i-start], widths, i == m.cursor, m.frame, i, m.workflowExpandable(r), m.expanded[workflowExpandKey(r)])
		}
		line.Hit = &rowHit{Kind: "workflow", Index: i, Key: r.identity(), X0: 0, X1: table + 2}
		body = append(body, line)
		if m.expanded[workflowExpandKey(r)] && m.workflowExpandable(r) {
			for _, label := range v.stepLabels {
				body = append(body, bodyLine{Text: "      " + subtleStyle.Render(truncate(label, max(0, table-6)))})
			}
		}
	}
	if len(m.rows) == 0 {
		message := "no workflows yet — kata cron workflow create --file <file> writes one"
		if m.query != "" {
			message = "no workflows match " + m.query
		}
		body = append(body, bodyLine{Text: "  " + subtleStyle.Render(truncate(message, max(0, m.width-2)))})
	}
	if width := inspector; width > 0 {
		if _, split := m.splitGeometry(); split {
			return body
		}
		if v := m.selectedWorkflow(); v != nil {
			body = beside(body, m.workflowInspector(v, width), table+2, 3)
		}
	}
	return body
}

// workflowExpandable reports whether a workflow row has steps to disclose,
// like a Kata issue with children.
func (m *model) workflowExpandable(r row) bool {
	v := m.wfIndex[workflowKey{r.projectUID, r.uid}]
	return v != nil && len(v.stepLabels) > 0 && v.laneAvailable && workflowDiagnosis(v.check) == ""
}

// workflowExpandKey keeps workflow expansion apart from run UIDs.
func workflowExpandKey(r row) string { return "workflow:" + r.identity() }

func workflowStepLabel(c workflowCheck, ordinal int) string {
	if c.Definition == nil || ordinal < 0 || ordinal >= len(c.Order) {
		return ""
	}
	for _, s := range c.Definition.Steps {
		if s.Key == c.Order[ordinal] {
			label := fmt.Sprintf("%d. %s  %s", ordinal+1, s.Key, s.Kind)
			if len(s.After) > 0 && (ordinal == 0 || len(s.After) != 1 || s.After[0] != c.Order[ordinal-1]) {
				label += " · after " + strings.Join(s.After, ",")
			}
			if s.Retries > 0 {
				label += fmt.Sprintf(" · retry %d", s.Retries)
			}
			return label
		}
	}
	return ""
}
func (m *model) workflowInspector(v *workflowView, width int) bodyLines {
	w := m.data.Workflows[v.index]
	c := v.check
	body := bodyLines{}
	line := func(text string, role lipgloss.Style) {
		body = append(body, inspectorLine(text, width, m.frame, role))
	}
	wrap := func(text string, role lipgloss.Style) {
		for _, s := range wrapText(text, width) {
			body = append(body, bodyLine{Text: role.Render(s)})
		}
	}
	header, dim := detailSectionHeaderStyle, dimStyle
	blank := func() { body = append(body, bodyLine{}) }
	field := func(label, value string) {
		l := inspectorLine(value, max(0, width-8), m.frame, detailMetaStyle)
		l.Text = header.Render(fitCell(label, 7)) + " " + l.Text
		for i := range l.Animations {
			l.Animations[i].X0 += 8
			l.Animations[i].X1 += 8
		}
		body = append(body, l)
	}
	heading := w.Name
	role := titleStyle
	if c.DecodeErr != nil {
		heading = "does not parse"
		role = failStyle
	} else if len(c.Unsupported) > 0 {
		heading = "unsupported workflow"
		role = failStyle
	}
	line(heading, role)
	blank()
	if m.showProjects() {
		field("project", w.Project)
	}
	if !v.laneAvailable {
		wrap("project unavailable", attentionStyle)
	}
	if c.DecodeErr != nil {
		wrap(c.DecodeErr.Error(), failStyle)
	}
	for _, reason := range c.Unsupported {
		wrap(reason, failStyle)
	}
	if workflowDiagnosis(c) != "" {
		wrap("fix it with e · r reloads", dim)
	}
	if def := c.Definition; def != nil {
		if def.About != "" {
			wrap(def.About, detailMetaStyle)
			blank()
		}
		input := def.Input
		if input == "" {
			input = "none"
		}
		field("input", input)
	}
	names := []string{}
	for _, j := range v.jobs {
		names = append(names, j.name)
	}
	field("jobs", fmt.Sprintf("%d", len(names))+" "+strings.Join(names, ", "))
	for _, warning := range c.Warnings {
		wrap("warn "+warning, attentionStyle)
	}
	for _, reason := range c.JobDependent {
		wrap("per job: "+reason, dim)
	}
	if c.Definition != nil {
		blank()
		body = append(body, bodyLine{Text: header.Render("Steps") + dim.Render(fmt.Sprintf(" %d", len(c.Definition.Steps)))})
		if c.Order == nil && len(c.Definition.Steps) > 0 {
			wrap("execution order unavailable", attentionStyle)
		}
		for i := 0; i < min(6, len(c.Order)); i++ {
			text := v.stepLabels[i]
			body = append(body, bodyLine{Text: workflowInspectorStep(text, width, dim)})
		}
		if len(c.Order) > 6 {
			body = append(body, bodyLine{Text: dim.Render(fmt.Sprintf("… %d more steps", len(c.Order)-6))})
		}
	}
	blank()
	line("Loaded history", header)
	if len(v.runs) == 0 {
		line("no loaded observations", dim)
	} else {
		r := m.data.Runs[v.runs[0]]
		label, role := runLabel(r)
		when := ago(m.now(), m.runView(r).When)
		if r.StartedAt != nil {
			when = ago(m.now(), *r.StartedAt)
		}
		body = append(body, bodyLine{Text: role.Render(label) + detailMetaStyle.Render(truncate(" · "+when, max(0, width-len(label))))})
		note, noteRole := runNote(r, m.now())
		wrap(note, noteRole)
	}
	return body
}

// workflowInspectorStep retains the source's dim number,
// the step name, then dim metadata aligned at the panel's right edge.
func workflowInspectorStep(label string, width int, dim lipgloss.Style) string {
	number, rest, ok := strings.Cut(label, ". ")
	if !ok {
		return truncate(label, width)
	}
	key, info, _ := strings.Cut(rest, "  ")
	number += "."
	keyWidth := width - len(number) - lipgloss.Width(info) - 2
	if keyWidth < 8 {
		info = truncate(info, max(width/3, 1))
		keyWidth = width - len(number) - lipgloss.Width(info) - 2
	}
	if keyWidth < 1 {
		return fitCell(number+" "+key, width)
	}
	return dim.Render(number) + " " + fitCell(key, keyWidth) + " " + dim.Render(info)
}
