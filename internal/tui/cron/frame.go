package cron

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *model) View() tea.View { return tea.NewView(m.renderFrame().Text) }
func (m *model) renderFrame() renderedFrame {
	if m.editor != nil {
		return renderedFrame{Text: m.editor.View()}
	}
	var body bodyLines
	help := jobsHelp()
	if m.currentView() == viewWorkflow {
		body = m.workflowDetail()
		help = workflowDetailHelp()
	} else if m.currentView() == viewRun {
		body = m.renderRunDetail()
		help = m.runDetailHelp()
	} else if m.currentView() == viewJob {
		body = m.renderJobDetail()
		help = m.jobDetailHelp()
	} else {
		switch m.tab {
		case 0:
			body = m.renderJobs()
		case 1:
			body = m.renderRuns()
			help = runsHelp()
		case 2:
			body = m.renderWorkflows()
			help = workflowHelp()
		}
	}
	bottom := bodyLines{}
	if m.currentView() == viewList && len(m.projectChoices) == 0 {
		_, _, page, pages := pager(len(m.rows), m.cursor, m.pageRows())
		extra := []string{}
		if m.tab == 0 {
			extra = m.jobsExtras()
		}
		if m.tab == 1 {
			if m.query != "" || m.status != "" {
				extra = append(extra, fmt.Sprintf("%d of %d match", len(m.rows), len(m.data.Runs)))
			}
			extra = append(extra, "loaded history")
			if m.data.MoreRuns {
				extra = append(extra, "] loads more")
			}
		}
		if m.tab == 2 && m.query != "" {
			extra = append(extra, fmt.Sprintf("%d of %d match", len(m.rows), len(m.wfIndex)))
		}
		bottom = append(bottom, bodyLine{Text: pageLabel(page, pages, extra...)})
	}
	peekStart := len(bottom)
	bottom = append(bottom, m.renderJobBox()...)
	bottom = append(bottom, m.footer()...)
	if len(m.projectChoices) == 0 && m.conflict == nil && len(m.confirm) == 0 {
		for _, line := range help.Lines(m.width) {
			bottom = append(bottom, bodyLine{Text: line})
		}
	}
	peek := m.renderPeek()
	if len(peek) > 0 {
		available := min(len(peek), max(0, m.height-len(bottom)))
		bottom = slices.Insert(bottom, peekStart, peek[:available]...)
	}
	if len(m.projectChoices) > 0 {
		body = bodyLines{{Text: "  " + detailSectionHeaderStyle.Render("new job in project")}, {Text: ""}}
		for i, key := range m.projectChoices {
			count := 0
			for _, j := range m.data.Jobs {
				if j.DeletedAt == nil && j.ProjectUID == key {
					count++
				}
			}
			prefix := "    "
			label := subtleStyle.Render(fitCell(m.projectName(key), 20))
			if i == m.projectCursor {
				prefix = "  " + cursorStyle.Render("▶ ")
				label = rowSelectedStyle.Render(fitCell(m.projectName(key), 20))
			}
			body = append(body, bodyLine{Text: prefix + label + subtleStyle.Render(fmt.Sprintf(" %d jobs", count)), Selected: i == m.projectCursor})
		}
		body = append(body, bodyLine{}, bodyLine{Text: "  " + footerHelp{Full: []helpItem{hintGroup("j/k", "move", 0, keys.Down, keys.Up, keys.ScrollUp, keys.ScrollDown), keys.Accept.hint(0), keys.Cancel.hint(0)}}.Render(m.width-2)})
	}
	scroll := &m.scroll
	follow := m.follow
	if len(m.projectChoices) > 0 {
		scroll, follow = &m.projectScroll, true
	}
	if _, split := m.splitGeometry(); split && len(m.projectChoices) == 0 {
		body = m.renderSplitBody(body, 0, len(bottom), scroll, follow)
		scroll = nil
		follow = false
	}
	p := pane{Body: body, Bottom: bottom, Width: m.width, Scroll: scroll, Follow: follow}
	if len(peek) > 0 && len(bottom) >= m.height {
		// A tiny selected-run view may have room only for its chrome and peek.
		// Avoid the pane's ordinary minimum body row displacing that footer.
		p = pane{Top: bottom, Width: m.width}
	}
	frame := p.Render(m.height)
	m.hits = frame.Hits
	return frame
}
func (m *model) footer() bodyLines {
	return footer(m.notice, m.data.Problems, searchState{Query: m.query, Editing: m.searching, Status: m.status})
}
func (m *model) pageRows() int {
	help := jobsHelp()
	switch m.tab {
	case 1:
		help = runsHelp()
	case 2:
		help = workflowHelp()
	}
	borders := 0
	if _, split := m.splitGeometry(); split {
		borders = 2
	}
	return max(3, m.height-2-len(help.Lines(m.width))-len(m.footer())-m.jobBoxHeight()-m.peekHeight()-borders)
}
func (m *model) pageSize() int { return max(1, m.height/2) }
func pager(total, cursor, rows int) (start, end, page, pages int) {
	rows = max(1, rows)
	total = max(0, total)
	pages = max(1, (total+rows-1)/rows)
	cursor = max(0, min(cursor, max(0, total-1)))
	start = cursor / rows * rows
	end = min(total, start+rows)
	page = cursor/rows + 1
	return
}

func pageLabel(page, pages int, extras ...string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "  page %d/%d", max(1, page), max(1, pages))
	for _, extra := range extras {
		if extra != "" {
			s.WriteString(" · " + plain(extra))
		}
	}
	return subtleStyle.Render(strings.TrimRight(s.String(), " "))
}
