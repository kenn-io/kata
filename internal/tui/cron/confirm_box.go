package cron

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	defs "go.kenn.io/kata/internal/tui/cron/definition"
	"go.kenn.io/kata/internal/tui/cron/jobform"
)

func (m *model) jobBoxHeight() int {
	if m.conflict == nil && len(m.confirm) == 0 {
		return 0
	}
	height := m.height
	if m.TabsVisible() {
		height += 3
	}
	return max(3, min(max(4, height/3), height-4-len(m.footer())-5))
}
func (m *model) renderJobConfirmation() bodyLines {
	heading := "delete " + clean(m.confirm[0].Name) + "?"
	body := bodyLines{{Text: detailSectionHeaderStyle.Render(heading)}}
	for _, j := range m.confirm {
		body = append(body, bodyLine{Text: "  " + subtleStyle.Render(clean(j.Project+" / "+j.Name))})
	}
	return append(body, bodyLine{Text: subtleStyle.Render("y delete · any other key cancels · run history is kept")})
}
func (m *model) renderJobConflict() bodyLines {
	c := m.conflict
	if c == nil {
		return nil
	}
	op := c.Operation
	if op == "" {
		op = "operation"
	}
	project := c.Pending.Project
	if project == "" {
		project = m.projectName(c.Pending.ProjectUID)
	}
	suffix := ""
	if project != "" {
		suffix = " · project " + project
	}
	var body bodyLines
	line := func(text string) {
		for _, part := range wrapText(text, max(1, m.width-2)) {
			body = append(body, bodyLine{Text: "  " + detailMetaStyle.Render(part)})
		}
	}
	if c.Newer.UID != "" {
		body = append(body, bodyLine{Text: attentionStyle.Render(clean("Kata has a newer definition of " + c.Pending.Name + suffix))})
		yours, err := defs.DecodeJob(c.Pending.Definition)
		theirs, newErr := defs.DecodeJob(c.Newer.Definition)
		if err != nil || newErr != nil {
			line(fmt.Sprintf("definition decode failed: yours: %v; Kata: %v", err, newErr))
			line("original definition guard kept")
		} else {
			for _, change := range jobform.Diff(yours, theirs, c.Pending.Name, c.Newer.Name, jobform.Context{Project: project, Workflows: m.data.Workflows, IssueReference: func(uid string) string { return m.knownIssueRef(c.Pending.ProjectUID, uid) }}) {
				line(change.Field + "  yours: " + change.Yours + "  Kata: " + change.Kata)
			}
		}
		line(op + " not applied · original definition guard kept")
	} else {
		body = append(body, bodyLine{Text: attentionStyle.Render(clean("Readback unavailable · " + op + " " + c.Pending.Name + suffix))})
		line("original definition guard kept")
		line("error: " + c.Error())
		if op == "delete" {
			line("outcome uncertain: the job may or may not be deleted")
		} else {
			line("outcome uncertain: the change may or may not have been applied")
		}
	}
	if op == "delete" {
		line(fmt.Sprintf("deleted %d: %s", len(c.Done), jobNames(c.Done)))
		line(fmt.Sprintf("pending %d: %s", len(c.Remaining), jobNames(c.Remaining)))
	}
	return append(body, bodyLine{Text: subtleStyle.Render("esc discards · j/k scroll · q quit")})
}
func (m *model) renderJobBox() bodyLines {
	height := m.jobBoxHeight()
	if height == 0 {
		return nil
	}
	var all bodyLines
	if m.conflict != nil {
		all = m.renderJobConflict()
	} else {
		all = m.renderJobConfirmation()
	}
	if len(all) == 0 {
		return nil
	}
	for i := range all {
		all[i].Text = ansi.Truncate(all[i].Text, max(0, m.width), "")
		all[i].Hit = nil
		all[i].Animations = nil
	}
	if height == 1 {
		return all[len(all)-1:]
	}
	header, footer := all[0], all[len(all)-1]
	middle := all[1 : len(all)-1]
	rows, _ := window(middle, max(0, height-2), &m.modalScroll, false)
	result := append(bodyLines{header}, rows...)
	for len(result) < height-1 {
		result = append(result, bodyLine{Text: strings.Repeat(" ", max(0, m.width))})
	}
	return append(result, footer)
}
