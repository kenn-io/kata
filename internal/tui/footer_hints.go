package tui

import (
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/helprender"
	"go.kenn.io/kit/tui/splitlayout"
)

func (m Model) helpRows() [][]helplayout.HelpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input)
	}
	if m.layout == splitlayout.Split {
		return m.splitHelpRows()
	}
	switch m.view {
	case viewDetail:
		return m.detail.detailHelpRows()
	case viewList:
		return m.list.queueHelpRows()
	case viewCredentials:
		return credentialsHelpRows()
	}
	return globalHelpRows()
}

func (m Model) queueHelpRows() [][]helplayout.HelpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input)
	}
	return m.list.queueHelpRows()
}

func (m Model) detailHelpRows() [][]helplayout.HelpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input)
	}
	return m.detail.detailHelpRows()
}

func (m Model) splitHelpRows() [][]helplayout.HelpItem {
	if m.modal != modalNone {
		return modalHelpRows(m.modal)
	}
	if m.input.kind != inputNone {
		return inputHelpRows(m.input)
	}
	if m.focus == focusDetail {
		return m.detail.detailHelpRows()
	}
	return m.list.queueHelpRows()
}

func listHelpRows(lm listModel, chrome viewChrome) [][]helplayout.HelpItem {
	if chrome.modal != modalNone {
		return modalHelpRows(chrome.modal)
	}
	if chrome.input.kind != inputNone {
		return inputHelpRows(chrome.input)
	}
	return appendUndoHint(lm.queueHelpRows(), chrome.undoAvailable)
}

func detailHelpRows(dm detailModel, chrome viewChrome) [][]helplayout.HelpItem {
	if chrome.modal != modalNone {
		return modalHelpRows(chrome.modal)
	}
	if chrome.input.kind != inputNone {
		return inputHelpRows(chrome.input)
	}
	return appendUndoHint(dm.detailHelpRows(), chrome.undoAvailable)
}

func appendUndoHint(rows [][]helplayout.HelpItem, available bool) [][]helplayout.HelpItem {
	if !available || len(rows) == 0 {
		return rows
	}
	out := make([][]helplayout.HelpItem, len(rows))
	copy(out, rows)
	out[0] = append(slices.Clone(rows[0]), helplayout.HelpItem{Key: "u", Description: "undo"})
	return out
}

func inputHelpRows(input inputState) [][]helplayout.HelpItem {
	if input.kind == inputSearchBar && input.searchFocus == searchFocusResults {
		return [][]helplayout.HelpItem{{
			{Key: "↑↓", Description: "move"},
			{Key: "enter", Description: "apply"},
			{Key: "esc", Description: "query"},
			{Key: "/", Description: "query"},
		}}
	}
	if input.kind == inputSearchBar {
		return [][]helplayout.HelpItem{{
			{Key: "↑↓/enter", Description: "results"},
			{Key: "esc", Description: "cancel"},
			{Key: "ctrl+u", Description: "clear"},
		}}
	}
	kind := input.kind
	switch {
	case kind.isPanelPrompt():
		return [][]helplayout.HelpItem{{
			{Key: "enter", Description: "commit"},
			{Key: "esc", Description: "cancel"},
		}}
	case kind == inputFilterForm:
		return [][]helplayout.HelpItem{{
			{Key: "ctrl+o", Description: "apply"},
			{Key: "esc", Description: "cancel"},
			{Key: "ctrl+r", Description: "reset"},
		}}
	case kind == inputNewIssueForm:
		return [][]helplayout.HelpItem{{
			{Key: "ctrl+o", Description: "create"},
			{Key: "esc", Description: "cancel"},
			{Key: "tab", Description: "field"},
			{Key: "ctrl+e", Description: "editor"},
		}}
	case kind.isCenteredForm():
		return [][]helplayout.HelpItem{{
			{Key: "ctrl+o", Description: "save"},
			{Key: "esc", Description: "cancel"},
			{Key: "ctrl+e", Description: "editor"},
		}}
	}
	return nil
}

func (lm listModel) queueHelpRows() [][]helplayout.HelpItem {
	row, ok := lm.targetQueueRow()
	items := []helplayout.HelpItem{
		{Key: "↑↓", Description: "move"},
		{Key: "↵", Description: "open"},
	}
	if ok && row.hasChildren {
		items = append(items, helplayout.HelpItem{Key: "space", Description: "expand"})
	}
	if lm.viewMode != issueListViewFlat && len(lm.expandableKeys()) > 0 {
		items = append(items, helplayout.HelpItem{Key: "E", Description: "all"})
	}
	items = append(items, helplayout.HelpItem{Key: "n", Description: "new"})
	if ok {
		items = append(items, helplayout.HelpItem{Key: "N", Description: "child"})
	}
	items = append(items,
		helplayout.HelpItem{Key: "/", Description: "search"},
		helplayout.HelpItem{Key: "f", Description: "filter"},
		helplayout.HelpItem{Key: "s", Description: "status"},
		helplayout.HelpItem{Key: "v", Description: "view"},
		helplayout.HelpItem{Key: "o", Description: "order"},
		helplayout.HelpItem{Key: "c", Description: "clear"},
		helplayout.HelpItem{Key: "x", Description: "close"},
		helplayout.HelpItem{Key: "!", Description: "priority"},
		helplayout.HelpItem{Key: "D", Description: "daemons"},
		helplayout.HelpItem{Key: "F", Description: "federation"},
		helplayout.HelpItem{Key: "C", Description: "credentials"},
		helplayout.HelpItem{Key: "L", Description: "layout"},
		helplayout.HelpItem{Key: "?", Description: "help"},
		helplayout.HelpItem{Key: "q", Description: "quit"},
	)
	return [][]helplayout.HelpItem{items}
}

// detailHelpRows is the persistent footer for the detail view. The
// detail surface is action-rich (edit/comment/label/owner/parent/
// blocker/link/close/reopen) and the user explicitly asked for the
// footer to be comprehensive — every key handled by the detail
// view's Update loop appears here so the user is never stranded
// looking for an action. The footer adapter wraps the row
// across multiple lines when the terminal is too narrow.
//
// Children focus swaps the navigation header (↑↓ child / ↵ open
// child / N new child / p parent) but keeps the same action surface
// because the same mutations apply to the parent issue regardless
// of which section the cursor is on.
func (dm detailModel) detailHelpRows() [][]helplayout.HelpItem {
	actions := []helplayout.HelpItem{
		{Key: "e", Description: "edit"},
		{Key: "c", Description: "comment"},
		{Key: "+", Description: "label"},
		{Key: "-", Description: "unlabel"},
		{Key: "a", Description: "owner"},
		{Key: "A", Description: "unassign"},
		{Key: "t", Description: "timed"},
		{Key: "x", Description: "close"},
		{Key: "r", Description: "reopen"},
		{Key: "p", Description: "parent"},
		{Key: "b", Description: "block"},
		{Key: "l", Description: "related"},
		{Key: "!", Description: "priority"},
		{Key: "D", Description: "daemons"},
		{Key: "F", Description: "federation"},
		{Key: "C", Description: "credentials"},
		{Key: "N", Description: "child"},
		{Key: "L", Description: "layout"},
		{Key: "esc", Description: "back"},
		{Key: "?", Description: "help"},
		{Key: "q", Description: "quit"},
	}
	if dm.detailFocus == focusChildren && len(dm.children) > 0 {
		nav := []helplayout.HelpItem{
			{Key: "↑↓", Description: "scroll"},
			{Key: "j/k", Description: "child"},
			{Key: "↵", Description: "open child"},
			{Key: "↹", Description: "section"},
			{Key: "pgup/pgdn", Description: "page"},
		}
		return [][]helplayout.HelpItem{append(nav, actions...)}
	}
	nav := []helplayout.HelpItem{
		{Key: "↑↓", Description: "scroll"},
		{Key: "j/k", Description: "row"},
		{Key: "↹", Description: "section"},
		{Key: "↵", Description: "open"},
		{Key: "pgup/pgdn", Description: "page"},
	}
	return [][]helplayout.HelpItem{append(nav, actions...)}
}

func modalHelpRows(kind modalKind) [][]helplayout.HelpItem {
	switch kind {
	case modalQuitConfirm:
		return [][]helplayout.HelpItem{{
			{Key: "y", Description: "confirm"},
			{Key: "n/esc", Description: "cancel"},
		}}
	case modalDiscardComment, modalDiscardNewIssue:
		return [][]helplayout.HelpItem{{
			{Key: "y", Description: "discard"},
			{Key: "n/esc", Description: "keep editing"},
		}}
	}
	return nil
}

func modalFirstHelpRows(kind modalKind, fallback [][]helplayout.HelpItem) [][]helplayout.HelpItem {
	if kind != modalNone {
		return modalHelpRows(kind)
	}
	return fallback
}

func renderAuxiliaryFooter(m Model, ordinary string) string {
	if rows := modalFirstHelpRows(m.modal, nil); rows != nil {
		return renderFooterHelpTable(rows, m.width)
	}
	return subtleStyle.Render(ordinary)
}

func globalHelpRows() [][]helplayout.HelpItem {
	return [][]helplayout.HelpItem{{
		{Key: "?", Description: "help"},
		{Key: "q", Description: "quit"},
	}}
}

func renderFooterHelpTable(rows [][]helplayout.HelpItem, width int) string {
	innerWidth := titleBarInnerWidth(width)
	body := helprender.RenderHelpTable(
		convertAndReflowHelpRows(rows, innerWidth),
		helprender.Styles{
			Key:         helpKeyStyle,
			Description: helpDescStyle,
			BorderColor: lipgloss.LightDark(activeHasDarkBackground)(
				lipgloss.Color("248"), lipgloss.Color("242")),
		},
	)
	if body == "" {
		return footerBarStyle.Render(padToWidth("", innerWidth))
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = footerBarStyle.Render(padToWidth(line, innerWidth))
	}
	return strings.Join(lines, "\n")
}

func helpLines(rows [][]helplayout.HelpItem, width int) int {
	innerWidth := titleBarInnerWidth(width)
	lines := len(convertAndReflowHelpRows(rows, innerWidth))
	if lines < 1 {
		return 1
	}
	return lines
}

func convertAndReflowHelpRows(
	rows [][]helplayout.HelpItem,
	width int,
) [][]helplayout.HelpItem {
	return helplayout.ReflowRows(rows, width, helprender.ColumnGap)
}
