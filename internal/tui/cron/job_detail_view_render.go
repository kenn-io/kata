package cron

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	defs "go.kenn.io/kata/internal/tui/cron/definition"
)

func jobDetailKVLine(label string, value []cell, width int) bodyLine {
	var text strings.Builder
	text.WriteString(subtleStyle.Render(fitCell(label, 16) + " "))
	for _, cell := range value {
		text.WriteString(cell.Style.Render(clean(cell.Text)))
	}
	return bodyLine{Text: ansi.Truncate(text.String(), max(0, width), "")}
}

func (m *model) buildJobDetail() *detail {
	d := newDetail(m.width, 16, detailSelection{Key: m.detailKey, Ordinal: m.detailOrdinal}, keepMissing, m.detailItemExists)
	d.Selectable("run")
	f := m.nav[len(m.nav)-1]
	v := m.selectedJobView()
	if v == nil {
		name := f.name
		if name == "" {
			name = "job"
		}
		d.Title(name, "", titleStyle)
		d.Styled(bodyLine{Text: "  " + failStyle.Render("deleted in Kata")})
		return d
	}
	j := v.def
	name := v.entry.Name
	if name == "" {
		name = "unnamed job"
	}
	d.Title(name, "", subtleStyle)
	if v.description != "" {
		d.Subtitle(v.description)
	}
	d.Styled(bodyLine{})
	if m.showProjects() {
		d.KV("project", m.projectName(v.key.projectUID))
	}
	d.KV("definition", "current · "+v.entry.DefinitionEventUID)
	d.KVStyled("tags", labelText(v.tags, max(0, m.width-17)), chipStyle)
	schedule := []cell{{Text: v.schedule, Style: detailMetaStyle}}
	d.Styled(jobDetailKVLine("schedule", schedule, m.width))
	catchup := j.Catchup
	if j.GraceSeconds > 0 {
		catchup += " · grace " + (time.Duration(j.GraceSeconds) * time.Second).String()
	}
	d.KV("catchup", catchup)
	d.KV("overlap", j.Overlap)
	state := []cell{{Text: "enabled", Style: detailMetaStyle}}
	if !j.Enabled {
		state = []cell{{Text: "paused", Style: attentionStyle}}
	}
	if v.shape == "recurring" {
		state = append(state, cell{Text: " · native (Kata delivers)", Style: detailMetaStyle})
	} else if v.decodeErr != nil {
		state = append(state, cell{Text: " · unsupported", Style: attentionStyle})
	}
	d.Styled(jobDetailKVLine("state", state, m.width))
	action := v.shape
	if action == "workflow" {
		action += " " + v.workflowName
	}
	if action == "notify" {
		action += " " + j.Action.Recipient
	}
	d.KV("action", action)
	if v.shape == "workflow" {
		workflow := v.workflowName
		if workflow == "" {
			workflow = "workflow unavailable"
		}
		d.KV("workflow", workflow)
		d.KV("input", v.input)
	}
	if j.Issue != nil {
		if v.shape == "recurring" {
			d.KV("parent", m.knownIssueRef(v.key.projectUID, j.Issue.ParentUID))
			d.KV("child title", j.Issue.Title)
		} else {
			issue := j.Issue.Kind + " " + m.knownIssueRef(v.key.projectUID, j.Issue.UID)
			if j.Issue.Kind == "per-run" {
				issue = `per-run "` + j.Issue.Title + `"`
				if deadline := j.Issue.DeadlineOffsetSeconds; deadline != nil {
					sign := ""
					if *deadline >= 0 {
						sign = "+"
					}
					issue += " · deadline " + sign + (time.Duration(*deadline) * time.Second).String()
				}
			}
			d.KV("issue", issue)
		}
	}
	if v.shape == "notify" || v.shape == "recurring" {
		d.KV("recipient", j.Action.Recipient)
	}
	execute := j.Action.Kind == "execute"
	if execute {
		d.KV("run parent option", m.knownIssueRef(v.key.projectUID, defs.RunParent(j)))
		d.KV("checkout key", j.CheckoutKey)
		if j.TimeoutSeconds > 0 {
			d.KV("timeout", (time.Duration(j.TimeoutSeconds) * time.Second).String())
		}
		variables := make([]string, 0, len(j.SecretRefs))
		for variable := range j.SecretRefs {
			variables = append(variables, variable)
		}
		sort.Strings(variables)
		var refs []cell
		for i, variable := range variables {
			if i > 0 {
				refs = append(refs, cell{Text: ", ", Style: detailMetaStyle})
			}
			name := j.SecretRefs[variable]
			role := detailMetaStyle
			refs = append(refs, cell{Text: variable + " ← ", Style: detailMetaStyle}, cell{Text: name, Style: role})
		}
		if len(refs) > 0 {
			d.Styled(jobDetailKVLine("secret refs", refs, m.width))
		}
	}
	if len(v.otherOptions) > 0 {
		d.KV("options", strings.Join(v.otherOptions, ", ")+" (preserved)")
	}
	payload := func(heading, text string) {
		d.Section(heading)
		if heading == "PROMPT" {
			lines := renderMarkdownLines(text, min(96, max(1, m.width-2)))
			for _, line := range lines[:min(6, len(lines))] {
				d.Styled(bodyLine{Text: "  " + line})
			}
			if len(lines) > 6 {
				d.Lines(fmt.Sprintf("… %d more lines", len(lines)-6))
			}
		} else {
			for _, line := range promptPreview(text, 6, max(0, m.width-2)) {
				d.Lines(line)
			}
		}
	}
	switch v.shape {
	case "prompt":
		payload("PROMPT", j.Action.Prompt)
	case "notify":
		payload("MESSAGE", j.Action.Message)
	case "recurring":
		if j.Issue != nil {
			payload("CHILD BODY", j.Issue.Body)
		}
		if j.Action.Message != "" {
			payload("MESSAGE", j.Action.Message)
		}
	case "workflow":
		d.Section("WORKFLOW")
		if v.workflow == nil {
			d.Lines("workflow unavailable")
		} else {
			if v.workflow.About != "" {
				d.Lines(v.workflow.About)
			}
			keys := []string{}
			for _, step := range v.workflow.Steps {
				keys = append(keys, step.Key)
			}
			unit := "steps"
			if len(keys) == 1 {
				unit = "step"
			}
			d.Lines(fmt.Sprintf("%d %s: %s", len(keys), unit, strings.Join(keys, " → ")))
		}
	}
	d.Section("RUNS · loaded history")
	items := make([]detailItem, 0, len(v.runIndexes))
	for _, index := range v.runIndexes {
		r := m.data.Runs[index]
		label, role := runLabel(r)
		trigger := m.runView(r).Trigger
		when := ""
		if r.StartedAt != nil {
			when = ago(m.now(), *r.StartedAt)
		}
		note, noteRole := runNote(r, m.now())
		items = append(items, detailItem{Key: detailItemKey("run", r.ProjectKey, r.UID), Cells: []cell{{Text: label, Style: role}, {Text: trigger, Style: detailMetaStyle}, {Text: when, Style: detailMetaStyle}, {Text: note, Style: noteRole}}})
	}
	if len(items) == 0 {
		d.Lines("no loaded observations; delivery may exist without a run observation")
	} else {
		// The Runs list sits as tight as the run list tab: its fixed cells
		// fit their content and the note takes the rest, scrolling when cut.
		rows := make([][]cell, len(items))
		for i := range items {
			rows[i] = items[i].Cells
		}
		widths := listWidths([]column{{Title: ""}, {Title: ""}, {Title: ""}, {Flex: true}}, rows, m.width)
		for i := range items {
			items[i].Widths = widths
		}
		d.Marquee(m.frame, "run")
		d.List("run", items, 20)
	}
	return d
}

func (m *model) renderJobDetail() bodyLines {
	d := m.buildDetail()
	s := d.Selection()
	m.detailKey, m.detailOrdinal = s.Key, s.Ordinal
	return d.Body()
}
