package cron

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type renderedFrame struct {
	Text     string
	Hits     hits
	BodyRows int
	Animates bool
}
type windowOptions struct{ focus int }
type windowOption func(*windowOptions)

func focusLine(n int) windowOption { return func(o *windowOptions) { o.focus = n } }
func block(text string) bodyLines {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	out := make(bodyLines, len(lines))
	for i, s := range lines {
		out[i] = bodyLine{Text: s}
	}
	return out
}
func window(body bodyLines, avail int, scroll *int, follow bool, opts ...windowOption) (bodyLines, int) {
	if avail <= 0 {
		return nil, 0
	}
	o := windowOptions{focus: -1}
	for _, opt := range opts {
		opt(&o)
	}
	view := avail
	if len(body) > avail && avail > 1 {
		view--
	}
	offset := 0
	if scroll != nil {
		offset = *scroll
	}
	offset = max(0, min(offset, max(0, len(body)-view)))
	if follow {
		focus := o.focus
		if focus < 0 {
			for i, line := range body {
				if line.Selected {
					focus = i
					break
				}
			}
		}
		if focus >= 0 && focus < len(body) {
			if view <= 2 {
				if focus < offset {
					offset = focus
				}
				if focus >= offset+view {
					offset = focus - view + 1
				}
			} else {
				if focus < offset+1 {
					offset = focus - 1
				}
				if focus > offset+view-2 {
					offset = focus - view + 2
				}
			}
			offset = max(0, min(offset, max(0, len(body)-view)))
		}
	}
	if scroll != nil {
		*scroll = offset
	}
	if len(body) == 0 {
		return nil, 0
	}
	end := min(len(body), offset+view)
	out := append(bodyLines(nil), body[offset:end]...)
	n := len(out)
	if len(body) > avail && avail > 1 {
		parts := []string{}
		if offset > 0 {
			parts = append(parts, fmt.Sprintf("↑ %d more", offset))
		}
		if end < len(body) {
			parts = append(parts, fmt.Sprintf("↓ %d more", len(body)-end))
		}
		out = append(out, bodyLine{Text: strings.Join(parts, "   ")})
	}
	return out, n
}

type pane struct {
	Top, Body, Bottom bodyLines
	Width             int
	Scroll            *int
	Follow            bool
}

func (p pane) Render(height int) renderedFrame {
	if height <= 0 {
		height = 24
	}
	if p.Width <= 0 {
		p.Width = 80
	}
	avail := max(1, height-len(p.Top)-len(p.Bottom))
	body, count := window(p.Body, avail, p.Scroll, p.Follow)
	all := append(bodyLines(nil), p.Top...)
	all = append(all, body...)
	for len(body) < avail {
		all = append(all, bodyLine{})
		body = append(body, bodyLine{})
	}
	all = append(all, p.Bottom...)
	if len(all) > height {
		all = all[:height]
	}
	frame := renderedFrame{Hits: hits{Rows: map[int]rowHit{}, Width: p.Width, Top: len(p.Top), BodyRows: count}, BodyRows: count}
	if p.Scroll != nil {
		frame.Hits.Scroll = *p.Scroll
	}
	texts := make([]string, len(all))
	for y, line := range all {
		texts[y] = ansi.Truncate(line.Text, p.Width, "")
		if line.Hit != nil && y >= len(p.Top) && y < len(p.Top)+count {
			hit := *line.Hit
			hit.X0 = max(0, hit.X0)
			hit.X1 = min(p.Width, hit.X1)
			if hit.X0 < hit.X1 {
				frame.Hits.Rows[y] = hit
			}
		}
		for _, span := range line.Animations {
			if max(0, span.X0) < min(p.Width, span.X1) {
				frame.Animates = true
			}
		}
	}
	frame.Text = strings.Join(texts, "\n")
	return frame
}

type rowHit struct {
	Kind   string
	Index  int
	Key    string
	X0, X1 int
}
type animationSpan struct{ X0, X1 int }
type bodyLine struct {
	Selected   bool
	Text       string
	Hit        *rowHit
	Animations []animationSpan
}
type bodyLines []bodyLine

func inspectorLine(text string, width, frame int, role lipgloss.Style) bodyLine {
	text = plain(text)
	line := bodyLine{Text: role.Render(marquee(text, width, frame))}
	if width > 0 && ansi.StringWidth(text) > width {
		line.Animations = []animationSpan{{0, width}}
	}
	return line
}
func inspectorKV(label, value string, width, frame int, labelWidth ...int) bodyLines {
	return inspectorKVCells(label, []cell{{Text: value, Style: detailMetaStyle}}, width, frame, labelWidth...)
}
func inspectorKVCells(label string, value []cell, width, frame int, labelWidth ...int) bodyLines {
	lw := 9
	if len(labelWidth) > 0 && labelWidth[0] > 0 {
		lw = labelWidth[0]
	}
	var text strings.Builder
	roles := []lipgloss.Style{}
	for _, cell := range value {
		s := plain(cell.Text)
		text.WriteString(s)
		for range []rune(s) {
			roles = append(roles, cell.Style)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return nil
	}
	w := max(0, width-lw-1)
	var b strings.Builder
	b.WriteString(subtleStyle.Render(fitCell(label, lw) + " "))
	for _, part := range marqueeWindow(text.String(), w, frame) {
		style := detailMetaStyle
		if part.index >= 0 {
			style = roles[part.index]
		}
		b.WriteString(style.Render(part.text))
	}
	line := bodyLine{Text: ansi.Truncate(b.String(), max(0, width), "")}
	if w > 0 && ansi.StringWidth(text.String()) > w {
		line.Animations = []animationSpan{{lw + 1, width}}
	}
	return bodyLines{line}
}
func clipSpan(span animationSpan, width int) (animationSpan, bool) {
	span.X0 = max(0, span.X0)
	span.X1 = min(width, span.X1)
	return span, span.X0 < span.X1
}
func beside(left, right bodyLines, leftWidth, gap int) bodyLines {
	leftWidth = max(0, leftWidth)
	gap = max(0, gap)
	out := make(bodyLines, max(len(left), len(right)))
	for i := range out {
		l, r := bodyLine{}, bodyLine{}
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		s := ansi.Truncate(l.Text, leftWidth, "")
		out[i].Selected = l.Selected
		out[i].Text = s + strings.Repeat(" ", max(0, leftWidth-ansi.StringWidth(s))+gap) + r.Text
		if l.Hit != nil {
			hit := *l.Hit
			if span, ok := clipSpan(animationSpan{hit.X0, hit.X1}, leftWidth); ok {
				hit.X0, hit.X1 = span.X0, span.X1
				out[i].Hit = &hit
			}
		}
		for _, span := range l.Animations {
			if span, ok := clipSpan(span, leftWidth); ok {
				out[i].Animations = append(out[i].Animations, span)
			}
		}
		for _, span := range r.Animations {
			if span.X0 < span.X1 {
				span.X0 += leftWidth + gap
				span.X1 += leftWidth + gap
				out[i].Animations = append(out[i].Animations, span)
			}
		}
	}
	return out
}

func (m *model) renderInspector(width int) bodyLines {
	if m.tab == 1 {
		if row, ok := m.selected(); ok {
			r := m.data.Runs[row.run]
			v := m.runView(r)
			body := bodyLines{inspectorLine(v.Job, width, m.frame, titleStyle), {}}
			if m.showProjects() {
				body = append(body, inspectorKV("project", m.projectName(r.ProjectKey), width, m.frame)...)
			}
			status, role := runLabel(r)
			body = append(body, inspectorKVCells("status", []cell{{Text: status, Style: role}}, width, m.frame)...)
			body = append(body, inspectorKV("trigger", v.Trigger, width, m.frame)...)
			detail, role := runDetailCell(r, m.now())
			body = append(body, inspectorKVCells("detail", []cell{{Text: detail, Style: role}}, width, m.frame)...)
			return body
		}
		return nil
	}
	if m.tab == 2 {
		if v := m.selectedWorkflow(); v != nil {
			return m.workflowInspector(v, width)
		}
		return nil
	}
	if m.tab == 0 {
		if r, ok := m.selected(); ok {
			return m.renderJobInspector(m.indexedJob(r), width)
		}
	}
	return nil
}
