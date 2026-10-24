package cron

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/records"
)

func (m *model) workflowDetail() bodyLines {
	d := m.buildWorkflowDetail()
	s := d.Selection()
	m.detailKey, m.detailOrdinal = s.Key, s.Ordinal
	return d.Body()
}

func (m *model) buildWorkflowDetail() *detail {
	d := newDetail(m.width, 14, detailSelection{Key: m.detailKey, Ordinal: m.detailOrdinal}, nearestSurvivor, m.detailItemExists)
	d.Marquee(m.frame, "used-by", "run")
	d.Selectable("step", "used-by", "run")
	v := m.selectedWorkflow()
	if v == nil {
		d.Title("workflow removed", "", subtleStyle)
		return d
	}
	w := m.data.Workflows[v.index]
	c := v.check
	d.Title(w.Name, "", titleStyle)
	d.KV("definition", "current · "+w.DefinitionEventUID)
	if !w.UpdatedAt.IsZero() {
		d.Subtitle("updated " + ago(m.now(), w.UpdatedAt))
	}

	problems := workflowDiagnosis(c) != "" || !v.laneAvailable
	if problems {
		d.Section("PROBLEMS")
		if c.DecodeErr != nil {
			workflowDetailStyled(d, "! does not parse: "+c.DecodeErr.Error(), m.width, failStyle)
		}
		if len(c.Unsupported) > 0 {
			workflowDetailStyled(d, "! unsupported workflow", m.width, failStyle)
			for _, reason := range c.Unsupported {
				workflowDetailStyled(d, "! "+reason, m.width, failStyle)
			}
		}
		if !v.laneAvailable {
			workflowDetailStyled(d, "lane unavailable", m.width, attentionStyle)
		}
		workflowDetailStyled(d, "fix it with e · r reloads", m.width, subtleStyle)
	}
	d.Styled(bodyLine{})
	if m.showProjects() {
		d.KV("project", m.projectName(v.key.projectUID))
	}

	if def := c.Definition; def != nil {
		if def.About != "" {
			for _, line := range wrapText(def.About, max(1, m.width-2)) {
				d.Lines(line)
			}
			d.Lines("")
		}
		input := def.Input
		if input == "" {
			input = "none"
		}
		d.KVWrap("input", input)
		for _, warning := range c.Warnings {
			workflowDetailStyled(d, "warning: "+warning, m.width, attentionStyle)
		}
		for _, reason := range c.JobDependent {
			workflowDetailStyled(d, "per job: "+reason, m.width, subtleStyle)
		}
		d.Section(fmt.Sprintf("steps %d", len(def.Steps)))
		if c.Order == nil && len(def.Steps) > 0 {
			workflowDetailStyled(d, "execution order unavailable", m.width, attentionStyle)
			for _, s := range def.Steps {
				d.Lines(s.Key + "  " + s.Kind)
				workflowDetailStyled(d, workflowStepText(s), m.width, subtleStyle)
			}
		} else {
			byKey := map[string]defs.Step{}
			for _, s := range def.Steps {
				byKey[s.Key] = s
			}
			items := []detailItem{}
			for i, key := range c.Order {
				s := byKey[key]
				itemKey := detailItemKey("step", v.key.projectUID, key)
				label := v.stepLabels[i]
				text := workflowStepText(s)
				lines := []string{truncate(strings.Split(text, "\n")[0], max(0, m.width-7))}
				if s.Kind == "prompt" {
					rendered := renderMarkdownLines(text, max(1, m.width-7))
					if len(rendered) > 0 {
						lines = rendered[:1]
					}
				}
				if m.expanded[itemKey] {
					wrapKey := workflowWrapKey{v.key, w.DefinitionEventUID, key, m.width}
					if m.workflowWraps == nil {
						m.workflowWraps = map[workflowWrapKey][]string{}
					}
					lines = m.workflowWraps[wrapKey]
					if lines == nil {
						if s.Kind == "prompt" {
							lines = renderMarkdownLines(text, max(1, m.width-7))
						} else {
							lines = wrapText(text, max(1, m.width-7))
						}
						m.workflowWraps[wrapKey] = lines
					}
				}
				after := bodyLines{}
				for _, line := range lines {
					styled := workflowPromptStyle(line)
					if s.Kind == "prompt" {
						styled = line
					}
					after = append(after, bodyLine{Text: "       " + styled})
				}
				items = append(items, detailItem{Key: itemKey, Expandable: true, Expanded: m.expanded[itemKey], Render: func(_ bool, width int) string { return workflowStepStyle(label, width) }, After: after})
			}
			d.List("step", items, -1)
		}
	}
	d.Section(fmt.Sprintf("USED BY %d", len(v.jobs)))
	cols := []column{{Title: "job", Width: 20}, {Title: "state", Width: 12}, {Title: "input", Flex: true, Width: 8}}
	if m.width >= 100 {
		cols = []column{{Title: "job", Width: 20}, {Title: "state", Width: 12}, {Title: "schedule", Width: 16}, {Title: "input", Flex: true, Width: 8}, {Title: "timeout", Width: 7}}
	}
	widths := layout(cols, max(1, m.width-3), 8)
	if len(v.jobs) == 0 {
		d.Lines("no jobs use this workflow")
	} else {
		headers := []cell{}
		for _, col := range cols {
			headers = append(headers, cell{Text: col.Title, Style: headerStyle})
		}
		d.Styled(tableRow(headers, widths, false, 0, -1))
		items := []detailItem{}
		for _, joined := range v.jobs {
			entry := m.data.Jobs[joined.index]
			j := m.jobs.byKey[jobKey{v.key.projectUID, entry.UID}]
			if j == nil {
				continue
			}
			state, role := workflowJobState(j)
			input := "—"
			if joined.input != "" {
				input = fmt.Sprintf("%q", joined.input)
			}
			cells := []cell{}
			for _, col := range cols {
				cell := cell{Style: detailMetaStyle}
				switch col.Title {
				case "job":
					cell.Text = joined.name
				case "state":
					cell.Text = state
					cell.Style = role
				case "schedule":
					cell.Text = j.schedule
				case "profile":
					cell.Text = joined.profile
				case "input":
					cell.Text = input
				case "timeout":
					cell.Text = "—"
					if joined.timeout > 0 {
						cell.Text = dur(joined.timeout)
					}
				}
				cells = append(cells, cell)
			}
			items = append(items, detailItem{Key: detailItemKey("used-by", v.key.projectUID, entry.UID), Cells: dimPlaceholders(cells), Widths: widths})
		}
		d.List("used-by", items, -1)
	}
	d.Section(fmt.Sprintf("RUNS %d · loaded history", len(v.runs)))
	if len(v.runs) == 0 {
		d.Lines("no loaded observations; history may be partial")
	} else {
		// The status cell fits the widest status word, needs-human.
		cols := []column{{Width: 11}, {Width: 9}, {Width: 9}, {Width: 18}, {Flex: true, Width: 8}}
		widths := layout(cols, max(1, m.width-3), 8)
		items := []detailItem{}
		for _, i := range v.runs {
			r := m.data.Runs[i]
			rv := m.runView(r)
			label, role := runLabel(r)
			when := ago(m.now(), m.runView(r).When)
			trigger := rv.Trigger
			note, noteRole := runNote(r, m.now())
			if records.Value(r.WorkflowDefinitionEventUID) != "" && records.Value(r.WorkflowDefinitionEventUID) != w.DefinitionEventUID {
				note += " · definition differs from current"
			}
			items = append(items, detailItem{Key: detailItemKey("run", r.ProjectKey, r.UID), Cells: []cell{{Text: label, Style: role}, {Text: trigger, Style: detailMetaStyle}, {Text: when, Style: detailMetaStyle}, {Text: rv.Job, Style: detailMetaStyle}, {Text: note, Style: noteRole}}, Widths: widths})
		}
		d.List("run", items, 20, "Runs shows loaded history only")
	}
	return d
}

func workflowDetailStyled(d *detail, text string, width int, role lipgloss.Style) {
	for _, line := range wrapText(text, max(1, width-2)) {
		d.Styled(bodyLine{Text: "  " + role.Render(line)})
	}
}

func workflowStepText(s defs.Step) string {
	if s.Kind == "command" {
		return s.Command
	}
	return s.Prompt
}

func workflowPromptStyle(text string) string {
	text = clean(text)
	out := ""
	for text != "" {
		start := strings.Index(text, "{{")
		if start < 0 {
			return out + subtleStyle.Render(text)
		}
		out += subtleStyle.Render(text[:start])
		text = text[start:]
		end := strings.Index(text, "}}")
		if end < 0 {
			return out + subtleStyle.Render(text)
		}
		token := text[:end+2]
		role := subtleStyle
		if token == "{{input}}" || token == "{{previous}}" {
			role = headerStyle
		}
		out += role.Render(token)
		text = text[end+2:]
	}
	return out
}

func workflowStepStyle(label string, width int) string {
	label = truncate(label, width)
	number, rest, found := strings.Cut(label, ". ")
	if !found {
		return label
	}
	key, kind, found := strings.Cut(rest, "  ")
	if !found {
		return subtleStyle.Render(number+". ") + rest
	}
	return subtleStyle.Render(number+". ") + key + "  " + chipStyle.Render(kind)
}

func workflowJobState(j *jobView) (string, lipgloss.Style) {
	switch {
	case !j.def.Enabled:
		return "paused", subtleStyle
	case j.decodeErr != nil:
		return "unsupported", attentionStyle
	default:
		return "enabled", detailMetaStyle
	}
}
