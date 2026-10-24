// Ported from herdr-kata c2a23deb internal/tui/table.go.

package cron

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type column struct {
	Title  string
	Width  int
	Flex   bool
	Shrink int
}
type cell struct {
	Text  string
	Style lipgloss.Style
}

func layout(cols []column, avail, minFlex int) []int {
	widths := make([]int, len(cols))
	if minFlex <= 0 {
		minFlex = 8
	}
	fixed, flex := 0, -1
	for i, c := range cols {
		widths[i] = max(0, c.Width)
		if c.Flex {
			flex = i
		} else {
			fixed += widths[i]
		}
	}
	gaps := max(0, len(cols)-1)
	if flex >= 0 {
		widths[flex] = max(minFlex, avail-fixed-gaps)
	}
	total := gaps
	order := make([]int, len(cols))
	for i, w := range widths {
		total += w
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		i, j := order[a], order[b]
		x, y := cols[i].Shrink, cols[j].Shrink
		if x > 0 && y > 0 && x != y {
			return x < y
		}
		if (x > 0) != (y > 0) {
			return x > 0
		}
		return i > j
	})
	for _, i := range order {
		cut := min(max(0, total-avail), max(0, widths[i]-3))
		widths[i] -= cut
		total -= cut
	}
	return widths
}

// ListWidths sizes a list the way Kata's issue list does (go.kenn.io/kata
// internal/tui/list_render.go): one-cell gutters, every fixed column as wide
// as its widest header or cell (capped at Width when set), and the flex column
// takes whatever is left of the pane after the two-cell row marker. When the
// pane is too narrow, the flex column gives way first, down to minListFlex;
// then columns with a positive Shrink, lowest first, down to three cells.
// A column with Shrink 0 never shrinks.
func listWidths(cols []column, rows [][]cell, width int) []int {
	widths := make([]int, len(cols))
	flex := -1
	used := max(0, len(cols)-1)
	for i, c := range cols {
		if c.Flex {
			flex = i
			continue
		}
		w := ansi.StringWidth(c.Title)
		for _, cells := range rows {
			if i < len(cells) {
				w = max(w, ansi.StringWidth(plain(cells[i].Text)))
			}
		}
		if c.Width > 0 {
			w = min(w, c.Width)
		}
		widths[i] = w
		used += w
	}
	avail := max(0, width-2)
	if flex >= 0 {
		widths[flex] = max(minListFlex, avail-used)
		used += widths[flex]
	}
	order := []int{}
	for i, c := range cols {
		if c.Shrink > 0 && !c.Flex {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return cols[order[a]].Shrink < cols[order[b]].Shrink })
	if flex >= 0 {
		order = append(order, flex)
	}
	for _, i := range order {
		cut := min(max(0, used-avail), max(0, widths[i]-3))
		widths[i] -= cut
		used -= cut
	}
	return widths
}

// tableWidth is the cell span of a list row: its columns plus their gutters.
func tableWidth(widths []int) int {
	total := max(0, len(widths)-1)
	for _, w := range widths {
		total += w
	}
	return total
}

// HeaderRow renders a list header as one span in Kata's tableHeaderStyle, so
// the gutter, the gaps and every title share the same bold, foreground and
// band.
func headerRow(cols []column, widths []int) bodyLine {
	var b strings.Builder
	b.WriteString("  ")
	for i, c := range cols {
		if i >= len(widths) {
			break
		}
		if i > 0 {
			b.WriteString(" ")
		}
		// A shrunk title clips without an ellipsis, as list cells do.
		b.WriteString(pad(ansi.Truncate(c.Title, max(0, widths[i]), ""), max(0, widths[i])))
	}
	return bodyLine{Text: tableHeaderStyle.Render(b.String())}
}

// TableRow uses the visible list ordinal for stable zebra bands; headers use -1.
func tableRow(cells []cell, widths []int, selected bool, frame, rowIndex int) bodyLine {
	if rowIndex < 0 {
		return renderRow(cells, widths, false, 0, false, -2, " ")
	}
	return renderRow(cells, widths, selected, frame, true, rowIndex, " ")
}

// Parent rows use one gutter glyph, including when selected.
func parentRow(cells []cell, widths []int, selected bool, frame, rowIndex int, hasChildren, expanded bool) bodyLine {
	marker := ""
	if hasChildren {
		marker = disclosure(true, expanded)
	}
	return renderRowWithMarker(cells, widths, selected, frame, true, rowIndex, " ", marker)
}

func disclosure(hasChildren, expanded bool) string {
	if !hasChildren {
		return " "
	}
	if expanded {
		return treeExpanded
	}
	return treeCollapsed
}
func staticRow(cells []cell, widths []int, selected bool) string {
	return renderRow(cells, widths, selected, 0, false, -1, " ").Text
}
func renderRow(cells []cell, widths []int, selected bool, frame int, animate bool, rowIndex int, tree string) bodyLine {
	return renderRowWithMarker(cells, widths, selected, frame, animate, rowIndex, tree, "")
}
func renderRowWithMarker(cells []cell, widths []int, selected bool, frame int, animate bool, rowIndex int, tree, marker string) bodyLine {
	var b strings.Builder
	line := bodyLine{Selected: selected}
	x := 2
	band := lipgloss.NewStyle()
	if rowIndex == -2 {
		band = tableHeaderStyle
	}
	if rowIndex >= 0 && rowIndex%2 == 1 {
		band = altRowStyle
	}
	if marker == "" {
		marker = " "
		if selected {
			marker = "▶"
		}
	}
	if selected {
		band = rowSelectedStyle
		b.WriteString(cursorStyle.Inherit(band).Render(marker))
		b.WriteString(band.Render(tree))
	} else {
		b.WriteString(band.Render(marker + tree))
	}
	for i, c := range cells {
		if i >= len(widths) {
			break
		}
		if i > 0 {
			b.WriteString(band.Render(" "))
			x++
		}
		width := max(0, widths[i])
		s := plain(c.Text)
		text := fitCell(s, width)
		if animate {
			text = marquee(s, width, frame)
			if width > 0 && ansi.StringWidth(s) > width {
				line.Animations = append(line.Animations, animationSpan{x, x + width})
			}
		}
		style := c.Style
		if selected {
			style = band
		}
		b.WriteString(style.Inherit(band).Render(text))
		x += width
	}
	line.Text = b.String()
	return line
}

// LabelText copies Kata f3a06c1 internal/tui/list_render.go's label-chip
// sorting and packing. Callers apply theme.Chip after their layout sanitizes
// the text, so selected table rows can still inherit their selection style.
func labelText(labels []string, available int) string {
	if len(labels) == 0 {
		return "(no labels)"
	}
	clean := make([]string, len(labels))
	for i, label := range labels {
		clean[i] = plain(label)
	}
	sort.Strings(clean)
	overflowReserve := 1 + ansi.StringWidth(fmt.Sprintf("+%d", len(clean)))
	packed := make([]string, 0, len(clean))
	used := 0
	for i, label := range clean {
		w := ansi.StringWidth(label) + 2
		if len(packed) > 0 {
			w++
		}
		needTail := 0
		if i < len(clean)-1 {
			needTail = overflowReserve
		}
		if used+w+needTail > available {
			break
		}
		packed = append(packed, "["+label+"]")
		used += w
	}
	if len(packed) == 0 {
		return fmt.Sprintf("[%d labels]", len(clean))
	}
	out := strings.Join(packed, " ")
	if dropped := len(clean) - len(packed); dropped > 0 {
		out += fmt.Sprintf(" +%d", dropped)
	}
	return out
}

// minListFlex is the floor a list's flex column keeps before fixed columns
// shrink: Kata's 20-cell title floor (listColumnWidths in list_render.go).
const minListFlex = 20
