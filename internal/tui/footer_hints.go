package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/mattn/go-runewidth"
)

// helpItem is a footer/help binding row: key + concise description.
type helpItem struct{ key, desc string }

func (m Model) helpRows() [][]helpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input.kind)
	}
	if m.layout == layoutSplit {
		return m.splitHelpRows()
	}
	switch m.view {
	case viewDetail:
		return m.detail.detailHelpRows()
	case viewList:
		return m.list.queueHelpRows()
	}
	return globalHelpRows()
}

func (m Model) queueHelpRows() [][]helpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input.kind)
	}
	return m.list.queueHelpRows()
}

func (m Model) detailHelpRows() [][]helpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input.kind)
	}
	return m.detail.detailHelpRows()
}

func (m Model) splitHelpRows() [][]helpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input.kind)
	}
	if m.focus == focusDetail {
		return m.detail.detailHelpRows()
	}
	return m.list.queueHelpRows()
}

func listHelpRows(lm listModel, chrome viewChrome) [][]helpItem {
	if chrome.input.kind != inputNone {
		return inputHelpRows(chrome.input.kind)
	}
	return lm.queueHelpRows()
}

func detailHelpRows(dm detailModel, chrome viewChrome) [][]helpItem {
	if chrome.input.kind != inputNone {
		return inputHelpRows(chrome.input.kind)
	}
	return dm.detailHelpRows()
}

func inputHelpRows(kind inputKind) [][]helpItem {
	switch {
	case kind.isCommandBar():
		return [][]helpItem{{
			{key: "enter", desc: "commit"},
			{key: "esc", desc: "cancel"},
			{key: "ctrl+u", desc: "clear"},
		}}
	case kind.isPanelPrompt():
		return [][]helpItem{{
			{key: "enter", desc: "commit"},
			{key: "esc", desc: "cancel"},
		}}
	case kind == inputFilterForm:
		return [][]helpItem{{
			{key: "ctrl+s", desc: "apply"},
			{key: "esc", desc: "cancel"},
			{key: "ctrl+r", desc: "reset"},
		}}
	case kind == inputNewIssueForm:
		return [][]helpItem{{
			{key: "ctrl+s", desc: "create"},
			{key: "esc", desc: "cancel"},
			{key: "tab", desc: "field"},
			{key: "ctrl+e", desc: "editor"},
		}}
	case kind.isCenteredForm():
		return [][]helpItem{{
			{key: "ctrl+s", desc: "save"},
			{key: "esc", desc: "cancel"},
			{key: "ctrl+e", desc: "editor"},
		}}
	}
	return nil
}

func (lm listModel) queueHelpRows() [][]helpItem {
	row, ok := lm.targetQueueRow()
	items := []helpItem{
		{key: "↑↓", desc: "move"},
		{key: "↵", desc: "open"},
	}
	if ok && row.hasChildren {
		items = append(items, helpItem{key: "space", desc: "expand"})
	}
	items = append(items, helpItem{key: "n", desc: "new"})
	if ok {
		items = append(items, helpItem{key: "N", desc: "child"})
	}
	items = append(items,
		helpItem{key: "/", desc: "search"},
		helpItem{key: "f", desc: "filter"},
		helpItem{key: "s", desc: "status"},
		helpItem{key: "o", desc: "order"},
		helpItem{key: "c", desc: "clear"},
		helpItem{key: "x", desc: "close"},
		helpItem{key: "!", desc: "priority"},
		helpItem{key: "D", desc: "daemons"},
		helpItem{key: "L", desc: "layout"},
		helpItem{key: "?", desc: "help"},
		helpItem{key: "q", desc: "quit"},
	)
	return [][]helpItem{items}
}

// detailHelpRows is the persistent footer for the detail view. The
// detail surface is action-rich (edit/comment/label/owner/parent/
// blocker/link/close/reopen) and the user explicitly asked for the
// footer to be comprehensive — every key handled by the detail
// view's Update loop appears here so the user is never stranded
// looking for an action. The reflowHelpRows packer wraps the row
// across multiple lines when the terminal is too narrow.
//
// Children focus swaps the navigation header (↑↓ child / ↵ open
// child / N new child / p parent) but keeps the same action surface
// because the same mutations apply to the parent issue regardless
// of which section the cursor is on.
func (dm detailModel) detailHelpRows() [][]helpItem {
	actions := []helpItem{
		{key: "e", desc: "edit"},
		{key: "c", desc: "comment"},
		{key: "+", desc: "label"},
		{key: "-", desc: "unlabel"},
		{key: "a", desc: "owner"},
		{key: "A", desc: "unassign"},
		{key: "x", desc: "close"},
		{key: "r", desc: "reopen"},
		{key: "p", desc: "parent"},
		{key: "b", desc: "block"},
		{key: "l", desc: "related"},
		{key: "!", desc: "priority"},
		{key: "D", desc: "daemons"},
		{key: "N", desc: "child"},
		{key: "L", desc: "layout"},
		{key: "esc", desc: "back"},
		{key: "?", desc: "help"},
		{key: "q", desc: "quit"},
	}
	if dm.detailFocus == focusChildren && len(dm.children) > 0 {
		nav := []helpItem{
			{key: "↑↓", desc: "scroll"},
			{key: "j/k", desc: "child"},
			{key: "↵", desc: "open child"},
			{key: "↹", desc: "section"},
			{key: "pgup/pgdn", desc: "page"},
		}
		return [][]helpItem{append(nav, actions...)}
	}
	nav := []helpItem{
		{key: "↑↓", desc: "scroll"},
		{key: "j/k", desc: "row"},
		{key: "↹", desc: "section"},
		{key: "↵", desc: "open"},
		{key: "pgup/pgdn", desc: "page"},
	}
	return [][]helpItem{append(nav, actions...)}
}

func modalHelpRows(kind modalKind) [][]helpItem {
	switch kind {
	case modalQuitConfirm:
		return [][]helpItem{{
			{key: "y", desc: "confirm"},
			{key: "n/esc", desc: "cancel"},
		}}
	}
	return nil
}

func globalHelpRows() [][]helpItem {
	return [][]helpItem{{
		{key: "?", desc: "help"},
		{key: "q", desc: "quit"},
	}}
}

func renderFooterHelpTable(rows [][]helpItem, width int) string {
	innerWidth := titleBarInnerWidth(width)
	body := renderHelpTable(rows, innerWidth)
	if body == "" {
		return footerBarStyle.Render(padToWidth("", innerWidth))
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = footerBarStyle.Render(padToWidth(line, innerWidth))
	}
	return strings.Join(lines, "\n")
}

func helpLines(rows [][]helpItem, width int) int {
	lines := len(reflowHelpRows(rows, titleBarInnerWidth(width)))
	if lines < 1 {
		return 1
	}
	return lines
}

// Adapted from roborev cmd/roborev/tui/tui.go.
func reflowHelpRows(rows [][]helpItem, width int) [][]helpItem {
	if width <= 0 {
		return rows
	}

	cellWidth := func(item helpItem) int {
		w := runewidth.StringWidth(item.key)
		if item.desc != "" {
			w += 1 + runewidth.StringWidth(item.desc)
		}
		return w
	}

	maxItemsPerRow := 0
	for _, row := range rows {
		if len(row) > maxItemsPerRow {
			maxItemsPerRow = len(row)
		}
	}

	for ncols := maxItemsPerRow; ncols >= 1; ncols-- {
		var candidate [][]helpItem
		for _, row := range rows {
			for i := 0; i < len(row); i += ncols {
				end := min(i+ncols, len(row))
				candidate = append(candidate, row[i:end])
			}
		}

		colW := make([]int, ncols)
		for _, crow := range candidate {
			for c, item := range crow {
				if w := cellWidth(item); w > colW[c] {
					colW[c] = w
				}
			}
		}

		total := 0
		for c, w := range colW {
			total += w
			if c > 0 {
				total += 2
			}
		}
		if total <= width {
			return candidate
		}
	}

	var result [][]helpItem
	for _, row := range rows {
		for _, item := range row {
			result = append(result, []helpItem{item})
		}
	}
	return result
}

func renderHelpTable(rows [][]helpItem, width int) string {
	rows = reflowHelpRows(rows, width)
	if len(rows) == 0 {
		return ""
	}

	borderColor := lipgloss.AdaptiveColor{Light: "248", Dark: "242"}
	cellStyle := lipgloss.NewStyle()
	cellWithBorder := lipgloss.NewStyle().
		PaddingLeft(1).
		Border(lipgloss.Border{Left: "▕"}, false, false, false, true).
		BorderForeground(borderColor)

	maxCols := 0
	for _, row := range rows {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}

	colMinW := make([]int, maxCols)
	for _, row := range rows {
		for c, item := range row {
			w := runewidth.StringWidth(item.key)
			if item.desc != "" {
				w += 1 + runewidth.StringWidth(item.desc)
			}
			if w > colMinW[c] {
				colMinW[c] = w
			}
		}
	}

	empty := make([][]bool, len(rows))
	t := table.New().
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderColumn(false).
		BorderRow(false).
		StyleFunc(func(row, col int) lipgloss.Style {
			minW := 0
			if col < len(colMinW) {
				minW = colMinW[col]
			}
			if col == 0 || (row < len(empty) && col < len(empty[row]) && empty[row][col]) {
				return cellStyle.Width(minW)
			}
			return cellWithBorder.Width(minW + 2)
		}).
		Wrap(false)

	for ri, row := range rows {
		styled := make([]string, maxCols)
		empty[ri] = make([]bool, maxCols)
		for i, item := range row {
			if item.desc != "" {
				styled[i] = helpKeyStyle.Render(item.key) + " " + helpDescStyle.Render(item.desc)
			} else {
				styled[i] = helpKeyStyle.Render(item.key)
			}
		}
		for i := len(row); i < maxCols; i++ {
			empty[ri][i] = true
		}
		t = t.Row(styled...)
	}

	return t.Render()
}
