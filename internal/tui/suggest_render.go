package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/mattn/go-runewidth"
	"go.kenn.io/kata/internal/textsafe"
)

// suggestMenuMaxRows caps the menu's visible-entry budget. The
// suggestion overlay sits above the info line; a tall menu would
// crowd the body/tab content. 6 entries plus borders is enough for
// the common case (a project's most-used labels) without dominating
// the detail view.
const suggestMenuMaxRows = 6

// suggestMenuMinWidth keeps the menu wide enough to be readable
// even when every label is short — the bordered box looks broken
// at narrower widths.
const suggestMenuMinWidth = 14

// suggestMenuMaxWidth caps the width so a single very-long label
// doesn't push the menu off the right edge of the panel. Truncates
// to this width with `…`.
const suggestMenuMaxWidth = 40

// renderSuggestMenu renders the autocomplete suggestion menu for the
// active label prompt. The menu is a bordered vertical list, each
// row formatted "label (count)" or just "label" when count==0 (the
// `-` prompt's source). Highlighted row uses selectedStyle for the
// in-list cursor.
//
// When the cache reports a placeholder state (loading / error /
// empty) the menu body is the placeholder text instead of an entry
// list. This keeps the menu present (non-zero footprint) so the
// detail layout doesn't snap-bounce as the cache transitions —
// loading -> populated re-uses the same menu height.
func renderSuggestMenu(
	s inputState, suggestions []LabelCount, entry labelCacheEntry,
) string {
	rows, hasEntries := suggestMenuRows(s, suggestions, entry)
	w := suggestMenuWidth(rows, hasEntries)
	body := strings.Join(rows, "\n")
	// w is the content width; +2 covers the side borders (Lip Gloss v2
	// sizes border-box) so rows are never re-wrapped by the box.
	box := lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(panelActiveBorder).
		Width(w + 2).
		Render(body)
	return box
}

// suggestMenuRows builds the per-row content (without borders /
// padding) for the menu. Returns the rows AND a bool indicating
// whether entries were rendered (so callers can choose between
// counting visible entries or placeholder rows when reserving height).
func suggestMenuRows(
	s inputState, suggestions []LabelCount, entry labelCacheEntry,
) ([]string, bool) {
	if rows, ok := placeholderRows(s, suggestions, entry); ok {
		return rows, false
	}
	visible, scroll := windowedSuggestions(suggestions, s.suggestHighlight, s.suggestScroll)
	rows := make([]string, len(visible))
	for i, v := range visible {
		rows[i] = renderSuggestRow(v, scroll+i == s.suggestHighlight)
	}
	return rows, true
}

// placeholderRows returns the menu body for non-entry states:
// loading / error / empty-cache / filtered-empty. A secondary bool
// ok=false means "render the entry list instead." For
// inputLabelPrompt, "empty cache" (no labels at all in the project)
// reads as a hint to type a fresh label; "filtered empty" (cache
// has labels but the buffer prefix matches none) reads as a "no
// match" hint so the user knows the typed prefix didn't hit
// anything they could pick.
func placeholderRows(
	s inputState, suggestions []LabelCount, entry labelCacheEntry,
) ([]string, bool) {
	if len(suggestions) > 0 {
		return nil, false
	}
	switch {
	case s.kind == inputLabelPrompt && entry.fetching:
		return []string{statusStyle.Render("loading…")}, true
	case s.kind == inputLabelPrompt && entry.err != nil:
		msg := textsafe.Line(entry.err.Error())
		return []string{errorStyle.Render("(error: " + msg + ")")}, true
	case s.kind == inputLabelPrompt && len(entry.labels) > 0:
		return []string{subtleStyle.Render("(no match — enter to add as new)")}, true
	case s.kind == inputLabelPrompt:
		return []string{
			subtleStyle.Render("(no labels in project — type to add)"),
		}, true
	case s.kind == inputRemoveLabelPrompt:
		return []string{subtleStyle.Render("(no labels attached)")}, true
	}
	return nil, false
}

// windowedSuggestions slices `all` into the currently-visible window
// based on the highlight cursor. The window scrolls so the highlighted
// entry is always visible — ensures keyboard navigation past the
// bottom of the visible window scrolls the menu rather than parking
// the cursor off-screen. Returns the visible slice AND the absolute
// index of its first row so callers can map highlight back to the
// rendered row.
func windowedSuggestions(all []LabelCount, highlight, scroll int) ([]LabelCount, int) {
	n := len(all)
	if n <= suggestMenuMaxRows {
		return all, 0
	}
	if highlight < scroll {
		scroll = highlight
	}
	if highlight >= scroll+suggestMenuMaxRows {
		scroll = highlight - suggestMenuMaxRows + 1
	}
	if scroll < 0 {
		scroll = 0
	}
	if scroll > n-suggestMenuMaxRows {
		scroll = n - suggestMenuMaxRows
	}
	return all[scroll : scroll+suggestMenuMaxRows], scroll
}

// renderSuggestRow formats a single entry row. Count is rendered in
// dim text (subtleStyle) on the right; count==0 omits the column so
// the `-` prompt's attached-labels list reads naturally without
// spurious "(0)" labels. The label is sanitized via textsafe.Line so
// agent-controlled label names cannot inject ANSI escapes.
func renderSuggestRow(lc LabelCount, highlighted bool) string {
	label := textsafe.Line(lc.Label)
	row := label
	if lc.Count > 0 {
		row = fmt.Sprintf("%s (%d)", label, lc.Count)
	}
	row = runewidth.Truncate(row, suggestMenuMaxWidth-2, "…")
	if highlighted {
		return selectedStyle.Render(row)
	}
	return row
}

// suggestMenuWidth picks the menu width: the widest row plus
// padding, clamped to [suggestMenuMinWidth, suggestMenuMaxWidth].
// Not the panel width — the menu is a small floating overlay, not a
// full-width row.
func suggestMenuWidth(rows []string, _ bool) int {
	w := suggestMenuMinWidth
	for _, r := range rows {
		rw := runewidth.StringWidth(stripANSI(r))
		if rw+2 > w {
			w = rw + 2
		}
	}
	if w > suggestMenuMaxWidth {
		w = suggestMenuMaxWidth
	}
	return w
}

// suggestMenuHeight returns the rendered menu height in rows
// (top border + body rows + bottom border). The body row count is
// max(visibleEntries, placeholderRows). overlaySuggestMenu uses this
// to compute the anchor row so the menu's bottom border lands exactly
// one row above the info line. The body+tab area is NOT shrunk by
// menuH — the menu overlays tab content; see detail_render.go::View.
func suggestMenuHeight(s inputState, suggestions []LabelCount, entry labelCacheEntry) int {
	rows, _ := suggestMenuRows(s, suggestions, entry)
	body := len(rows)
	if body < 1 {
		body = 1
	}
	return body + 2 // top + bottom borders
}

// overlayAtCorner splices a panel onto bg at (anchorRow, anchorCol).
// Mirrors quit_modal.go::overlayModal but with explicit placement
// instead of centering — the suggestion menu is right-anchored above
// the info line, not centered. ANSI-aware row splicing uses the
// same helpers as overlayModal so styled bg lines (lipgloss output)
// don't get mis-counted by escape bytes.
//
// width / height are for the underlying terminal; bg is the already-
// rendered view. anchorRow / anchorCol are the panel's top-left
// corner in cell coordinates. Out-of-range placement (negative,
// past-edge) clamps to the visible area.
func overlayAtCorner(
	bg, panel string, width, height, anchorRow, anchorCol int,
) string {
	if panel == "" {
		return bg
	}
	bgLines := strings.Split(bg, "\n")
	for len(bgLines) < height {
		bgLines = append(bgLines, "")
	}
	panelLines := strings.Split(panel, "\n")
	panelW := lipgloss.Width(panel)
	row, col := anchorRow, anchorCol
	if row < 0 {
		row = 0
	}
	if col < 0 {
		col = 0
	}
	if col+panelW > width {
		col = width - panelW
	}
	if col < 0 {
		col = 0
	}
	for i, pLine := range panelLines {
		idx := row + i
		if idx < 0 || idx >= len(bgLines) {
			continue
		}
		bgLines[idx] = spliceRow(bgLines[idx], pLine, col, panelW)
	}
	return strings.Join(bgLines, "\n")
}

// spliceRow places `panelLine` onto `bg` starting at column `col`
// (cell-aware). The bg cells under the panel are replaced; cells to
// the right (after col+panelW) are preserved. ANSI escapes in the
// bg string are passed through verbatim so styling carries past the
// splice point.
func spliceRow(bg, panelLine string, col, panelW int) string {
	var b strings.Builder
	if col > 0 {
		left, leftWidth := ansiAwarePrefix(bg, col)
		b.WriteString(left)
		if leftWidth < col {
			b.WriteString(strings.Repeat(" ", col-leftWidth))
		}
	}
	b.WriteString(panelLine)
	rightStart := col + panelW
	b.WriteString(ansiAwareSuffix(bg, rightStart))
	return b.String()
}
