package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

type helpSection struct {
	title string
	rows  []helpItem
}

// helpSections returns bindings grouped by section in stable order.
// TestHelpSections_AllBindingsCovered fails CI when a keymap entry is
// missed here, so future binding additions must update this too.
func helpSections(km keymap) []helpSection {
	r := func(k key) helpItem { return helpItem{keyDisplay(k), k.Help} }
	return []helpSection{
		{"Global", []helpItem{r(km.Help), r(km.Quit), r(km.Projects), r(km.Daemons), r(km.ToggleLayout)}},
		{"Graph", []helpItem{
			r(km.Up), r(km.Down), r(km.ScrollUp), r(km.ScrollDown),
			r(km.PageUp), r(km.PageDown), r(km.Home),
			r(km.End), r(km.Open), r(km.ExpandCollapse),
			r(km.Expand), r(km.Collapse), r(km.NewIssue),
			r(km.SortChildren), r(km.Close), r(km.Reopen),
		}},
		{"Detail", []helpItem{
			r(km.NextTab), r(km.PrevTab), r(km.JumpRef), r(km.Back),
			r(km.EditBody), r(km.NewComment), r(km.SetParent),
			r(km.AddBlocker), r(km.AddLink), r(km.AddLabel),
			r(km.RemoveLabel), r(km.AssignOwner), r(km.ClearOwner),
			r(km.SetPriority),
		}},
		{"Children", []helpItem{
			r(km.NewChild),
			{key: "j/k", desc: "move child cursor"},
			{key: "↑↓", desc: "scroll detail viewport"},
			{key: "enter", desc: "open child"},
		}},
		{"Forms", []helpItem{
			{key: "ctrl+s", desc: "save or apply"},
			{key: "esc", desc: "cancel"},
			{key: "tab/shift+tab", desc: "change field"},
			{key: "ctrl+e", desc: "open editor"},
			{key: "ctrl+u", desc: "clear prompt"},
		}},
		{"Filters", []helpItem{
			r(km.Search), r(km.FilterStatus), r(km.FilterForm), r(km.ClearFilters),
			{key: "ctrl+r", desc: "reset filter form"},
		}},
	}
}

// keyDisplay joins multi-key bindings with '/' (e.g. "q/ctrl+c").
func keyDisplay(k key) string {
	parts := make([]string, len(k.Keys))
	for i, binding := range k.Keys {
		if binding == " " {
			parts[i] = "space"
		} else {
			parts[i] = binding
		}
	}
	return strings.Join(parts, "/")
}

// renderHelp builds the help overlay. width picks column count.
func renderHelp(km keymap, width int, filter ListFilter) string {
	cols := chunkSections(helpSections(km), helpColumnCount(width))
	rendered := make([]string, len(cols))
	for i, g := range cols {
		rendered[i] = renderHelpGroup(g)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, padColumns(rendered)...)
	parts := []string{titleStyle.Render("kata — keybindings"), ""}
	if chips := renderChips(filter); chips != "" {
		parts = append(parts, chips, "")
	}
	return strings.Join(append(parts, body, "",
		subtleStyle.Render("press ? to return")), "\n")
}

// helpColumnCount picks how many on-screen columns the layout uses at
// width w. Wide terminals get the sections side-by-side; narrow ones
// stack everything in a single column.
func helpColumnCount(w int) int {
	switch {
	case w >= 120:
		return 3
	case w >= 80:
		return 2
	}
	return 1
}

// chunkSections splits sections into cols on-screen columns by packing
// sections vertically per column (ceil(len/cols) sections per column).
// cols < 1 is clamped so a zero/negative width still renders something.
//
// Earlier this function treated the second argument as "sections per
// chunk", which inverted the layout: helpColumnCount(120)=3 produced one
// chunk of three sections (one screen column) instead of three single-
// section chunks (three screen columns). The reframe matches the
// helpColumnCount contract.
func chunkSections(s []helpSection, cols int) [][]helpSection {
	if cols < 1 {
		cols = 1
	}
	perCol := (len(s) + cols - 1) / cols
	if perCol < 1 {
		perCol = 1
	}
	out := [][]helpSection{}
	for i := 0; i < len(s); i += perCol {
		out = append(out, s[i:min(i+perCol, len(s))])
	}
	return out
}

// renderHelpGroup formats one row-chunk as a single column: bold title
// per section + 'key  desc' lines with keys padded to a uniform width.
func renderHelpGroup(group []helpSection) string {
	parts := []string{}
	for i, s := range group {
		if i > 0 {
			parts = append(parts, "")
		}
		parts = append(parts, titleStyle.Render(s.title))
		keyW := 0
		for _, r := range s.rows {
			if w := runewidth.StringWidth(r.key); w > keyW {
				keyW = w
			}
		}
		for _, r := range s.rows {
			pad := strings.Repeat(" ", keyW-runewidth.StringWidth(r.key)+2)
			parts = append(parts, helpKeyStyle.Render(r.key)+pad+
				helpDescStyle.Render(r.desc))
		}
	}
	return strings.Join(parts, "\n")
}

// padColumns equalizes column heights and appends a 4-space gutter to
// every line of every non-final column so JoinHorizontal never abuts
// the next column's content with no separation. Without the per-line
// gutter, a long line in column N (e.g. "toggle split/stacked layout")
// runs straight into column N+1's first row (roborev #17173 finding 2);
// padding only the trailing blank line — the previous behaviour — left
// the long row gutter-less.
func padColumns(cols []string) []string {
	maxN := 0
	for _, c := range cols {
		if n := strings.Count(c, "\n"); n > maxN {
			maxN = n
		}
	}
	out := make([]string, len(cols))
	for i, c := range cols {
		padded := c + strings.Repeat("\n", maxN-strings.Count(c, "\n"))
		if i == len(cols)-1 {
			out[i] = padded
			continue
		}
		lines := strings.Split(padded, "\n")
		for j := range lines {
			lines[j] += "    "
		}
		out[i] = strings.Join(lines, "\n")
	}
	return out
}
