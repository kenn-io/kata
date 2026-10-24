package cron

import (
	"fmt"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type detailSelection struct {
	Key     string
	Ordinal int
}
type missingPolicy int

const (
	keepMissing missingPolicy = iota
	nearestSurvivor
	noSelection
)

func resolveSelection(current detailSelection, items []rowHit, policy missingPolicy, exists func(string) bool) detailSelection {
	if policy == noSelection {
		return detailSelection{}
	}
	for i, item := range items {
		if item.Key == current.Key {
			return detailSelection{item.Key, i}
		}
	}
	if current.Key != "" && (exists == nil || exists(current.Key) || policy == keepMissing) {
		return current
	}
	if len(items) == 0 {
		return detailSelection{}
	}
	ordinal := 0
	if current.Key != "" {
		ordinal = max(0, min(current.Ordinal, len(items)-1))
	}
	return detailSelection{items[ordinal].Key, ordinal}
}
func moveSelection(current detailSelection, items []rowHit, delta int) detailSelection {
	if delta == 0 || len(items) == 0 {
		return current
	}
	ordinal := current.Ordinal
	found := false
	for i, item := range items {
		if item.Key == current.Key {
			ordinal = i
			found = true
			break
		}
	}
	ordinal += delta
	if !found && delta > 0 {
		ordinal--
	}
	ordinal = max(0, min(ordinal, len(items)-1))
	return detailSelection{items[ordinal].Key, ordinal}
}

type detailItem struct {
	Key                  string
	Expandable, Expanded bool
	Cells                []cell
	Widths               []int
	Render               func(selected bool, width int) string
	After                bodyLines
}
type detailEntry struct {
	lines   bodyLines
	kind    string
	items   []detailItem
	limit   int
	capNote string
}
type detail struct {
	width, labelWidth int
	selection         detailSelection
	policy            missingPolicy
	exists            func(string) bool
	selectable        map[string]bool
	entries           []detailEntry
	frame             int
	marqueeKinds      map[string]bool
	built             bool
	body              bodyLines
	items             []rowHit
}

func newDetail(width, labelWidth int, selection detailSelection, policy missingPolicy, exists func(string) bool) *detail {
	if labelWidth <= 0 {
		labelWidth = 14
	}
	return &detail{width: max(0, width), labelWidth: labelWidth, selection: selection, policy: policy, exists: exists, selectable: map[string]bool{}}
}

// Marquee opts table lists into the board's shared animation counter.
func (d *detail) Marquee(frame int, kinds ...string) {
	d.frame = frame
	d.marqueeKinds = map[string]bool{}
	for _, kind := range kinds {
		d.marqueeKinds[kind] = true
	}
	d.built = false
}
func (d *detail) Selectable(kinds ...string) {
	for _, kind := range kinds {
		d.selectable[kind] = true
	}
	d.built = false
}
func (d *detail) append(lines bodyLines) {
	d.entries = append(d.entries, detailEntry{lines: lines})
	d.built = false
}
func (d *detail) Title(text, label string, role lipgloss.Style) {
	label = plain(label)
	width := d.width
	if label != "" {
		width = max(0, width-ansi.StringWidth(label)-1)
	}
	s := titleStyle.Render(truncate(text, width))
	if label != "" {
		if s != "" {
			s += " "
		}
		s += role.Render(label)
	}
	d.append(bodyLines{{Text: ansi.Truncate(s, d.width, "")}})
}
func (d *detail) Subtitle(lines ...string) {
	for _, line := range lines {
		d.append(bodyLines{{Text: subtleStyle.Render(truncate(line, max(0, d.width)))}})
	}
}
func (d *detail) KV(label, value string) { d.KVStyled(label, value, detailMetaStyle) }
func (d *detail) KVStyled(label, value string, role lipgloss.Style) {
	if plain(value) == "" {
		return
	}
	d.append(bodyLines{{Text: subtleStyle.Render(fitCell(label, d.labelWidth)) + " " + role.Render(truncate(value, max(0, d.width-d.labelWidth-1)))}})
}
func (d *detail) KVWrap(label, value string) {
	if plain(value) == "" {
		return
	}
	for i, line := range wrapText(value, max(1, d.width-d.labelWidth-1)) {
		prefix := strings.Repeat(" ", d.labelWidth)
		if i == 0 {
			prefix = fitCell(label, d.labelWidth)
		}
		d.append(bodyLines{{Text: subtleStyle.Render(prefix) + " " + detailMetaStyle.Render(line)}})
	}
}
func (d *detail) Section(name string) {
	label := []rune(strings.ToLower(plain(name)))
	if len(label) > 0 {
		label[0] = unicode.ToUpper(label[0])
	}
	d.append(bodyLines{{}, {Text: detailSectionHeaderStyle.Render(string(label))}})
}
func (d *detail) Lines(text string) {
	for line := range strings.SplitSeq(text, "\n") {
		d.append(bodyLines{{Text: "  " + truncate(line, max(0, d.width-2))}})
	}
}
func staticLines(lines bodyLines) bodyLines {
	out := make(bodyLines, len(lines))
	for i, line := range lines {
		out[i] = bodyLine{Text: line.Text}
	}
	return out
}
func (d *detail) Styled(lines ...bodyLine) { d.append(staticLines(lines)) }
func (d *detail) List(kind string, items []detailItem, limit int, capNote ...string) {
	if limit == 0 {
		limit = 20
	}
	note := ""
	if len(capNote) > 0 {
		note = plain(capNote[0])
	}
	d.entries = append(d.entries, detailEntry{kind: kind, items: append([]detailItem(nil), items...), limit: limit, capNote: note})
	d.built = false
}
func (d *detail) build() {
	if d.built {
		return
	}
	d.body = nil
	d.items = nil
	selection := d.selection
	supplied := false
	for _, entry := range d.entries {
		if entry.kind == "" {
			continue
		}
		n := len(entry.items)
		if entry.limit > 0 {
			n = min(n, entry.limit)
		}
		selectable := d.selectable[entry.kind] && d.policy != noSelection
		if selectable {
			boundary := len(d.items) + n
			for i, item := range entry.items {
				if item.Key == selection.Key {
					supplied = true
					if i >= n {
						selection.Ordinal = boundary
					}
				}
			}
			for _, item := range entry.items[:n] {
				d.items = append(d.items, rowHit{Kind: entry.kind, Key: item.Key, Index: len(d.items), X0: 0, X1: d.width})
			}
		}
	}
	exists := d.exists
	if supplied {
		exists = func(string) bool { return true }
	}
	d.selection = resolveSelection(selection, d.items, d.policy, exists)
	index := 0
	for _, entry := range d.entries {
		if entry.kind == "" {
			d.body = append(d.body, entry.lines...)
			continue
		}
		n := len(entry.items)
		if entry.limit > 0 {
			n = min(n, entry.limit)
		}
		selectable := d.selectable[entry.kind] && d.policy != noSelection
		for _, item := range entry.items[:n] {
			selected := selectable && item.Key == d.selection.Key
			line := bodyLine{}
			if item.Render != nil {
				text := ansi.Truncate(item.Render(selected, max(0, d.width-2)), max(0, d.width-2), "")
				if selected {
					text = itemSelectedStyle.Render(ansi.Strip(text))
					line.Text = cursorStyle.Inherit(itemSelectedStyle).Render("▶") + itemSelectedStyle.Render(disclosure(item.Expandable, item.Expanded)) + text
				} else {
					line.Text = " " + disclosure(item.Expandable, item.Expanded) + text
				}
			} else {
				if d.marqueeKinds[entry.kind] {
					line = renderRow(item.Cells, item.Widths, selected, d.frame, true, -1, disclosure(item.Expandable, item.Expanded))
				} else {
					line.Text = staticRow(item.Cells, item.Widths, selected)
				}
			}
			line.Selected = selected
			line.Text = ansi.Truncate(line.Text, d.width, "")
			if selectable && index < len(d.items) {
				hit := d.items[index]
				line.Hit = &hit
				index++
			}
			d.body = append(d.body, line)
			d.body = append(d.body, staticLines(item.After)...)
		}
		if n < len(entry.items) {
			text := fmt.Sprintf("  +%d older", len(entry.items)-n)
			if entry.capNote != "" {
				text += " · " + entry.capNote
			}
			d.body = append(d.body, bodyLine{Text: subtleStyle.Render(text)})
		}
	}
	d.built = true
}
func (d *detail) Body() bodyLines            { d.build(); return d.body }
func (d *detail) Items() []rowHit            { d.build(); return d.items }
func (d *detail) Selection() detailSelection { d.build(); return d.selection }
