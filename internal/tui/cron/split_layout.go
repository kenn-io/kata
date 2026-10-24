package cron

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"go.kenn.io/kit/tui/splitlayout"
)

// Keep the custom table and passive inspector inside Kata's pane geometry.
var splitConfig = splitlayout.Config{ListMinWidth: 68, ListMaxWidth: 110, DetailReservedWidth: 48, DetailMinWidth: 20, MinBodyHeight: 4}

func (m *model) splitGeometry() (splitlayout.Geom, bool) {
	if m.currentView() != viewList || m.listOnly || splitlayout.PickLayout(m.width, m.height) != splitlayout.Split {
		return splitlayout.Geom{}, false
	}
	if _, ok := m.selected(); !ok {
		return splitlayout.Geom{}, false
	}
	return splitConfig.Geometry(m.width, m.height, 0), true
}

func (m *model) listWidth() int {
	if geometry, ok := m.splitGeometry(); ok {
		return geometry.ListInnerW
	}
	return m.width
}

func (m *model) renderSplitBody(list bodyLines, topRows, bottomRows int, scroll *int, follow bool) bodyLines {
	geometry := splitConfig.Geometry(m.width, m.height, topRows+bottomRows-2)
	left, _ := window(list, geometry.ListInnerH, scroll, follow)
	right, _ := window(m.renderInspector(geometry.DetailInnerW), geometry.DetailInnerH, nil, false)
	text := func(body bodyLines) string {
		lines := make([]string, len(body))
		for i, line := range body {
			lines[i] = line.Text
		}
		return strings.Join(lines, "\n")
	}
	a := strings.Split(splitlayout.PaneStyle(panelActiveBorder, geometry.ListOuterW, geometry.BodyH).Render(text(left)), "\n")
	b := strings.Split(splitlayout.PaneStyle(panelInactiveBorder, geometry.DetailOuterW, geometry.BodyH).Render(text(right)), "\n")
	out := make(bodyLines, geometry.BodyH)
	for i := range out {
		out[i].Text = a[i] + b[i]
	}
	for i, line := range left {
		if i+1 >= len(out)-1 {
			break
		}
		if line.Hit != nil {
			hit := *line.Hit
			hit.X0 = max(0, hit.X0) + 1
			hit.X1 = min(geometry.ListInnerW, hit.X1) + 1
			if hit.X0 < hit.X1 {
				out[i+1].Hit = &hit
			}
		}
		for _, span := range line.Animations {
			if clipped, ok := clipSpan(span, geometry.ListInnerW); ok {
				clipped.X0++
				clipped.X1++
				out[i+1].Animations = append(out[i+1].Animations, clipped)
			}
		}
	}
	// Inspector animation stays passive and never adds a row hit.
	for i, line := range right {
		if i+1 >= len(out)-1 {
			break
		}
		for _, span := range line.Animations {
			if clipped, ok := clipSpan(span, geometry.DetailInnerW); ok {
				clipped.X0 += geometry.ListOuterW + 1
				clipped.X1 += geometry.ListOuterW + 1
				out[i+1].Animations = append(out[i+1].Animations, clipped)
			}
		}
	}
	for i := range out {
		out[i].Text = ansi.Truncate(out[i].Text, m.width, "")
	}
	return out
}
