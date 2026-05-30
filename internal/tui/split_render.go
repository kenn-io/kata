package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderSplit composes the M6 split-pane layout: a 68-cell list
// pane on the left + flex detail pane on the right, sharing a
// single top-line title bar and bottom-line info+footer rows. Each
// pane is bordered; the focused pane's border uses panelActiveBorder,
// the inactive pane uses panelInactiveBorder so the user always sees
// which pane owns key dispatch.
//
// Modal overlays (filter form, new-issue form, quit confirm,
// suggestion menu) render OVER the whole composition — the modal
// machinery in Model.View applies after this returns. The caller
// (Model.viewBody) only invokes renderSplit when m.layout ==
// layoutSplit; the M5 too-narrow short-circuit runs ahead of this
// path, so width/height are guaranteed >= split breakpoints.
func renderSplit(m Model) string {
	width, height := m.width, m.height
	chrome := m.chrome()
	title := renderTitleBar(width, chrome.scope, chrome.version, chrome.daemon)
	helpRows := m.splitHelpRows()
	footerLines := helpLines(helpRows, width)
	footer := renderSplitFooter(width, m)
	infoLine := renderSplitInfoLine(width, m)
	// Body = (height - title - infoLine - adaptive footer) rows. The
	// two panes share that vertical budget; they're rendered
	// side-by-side then joined column-wise with lipgloss.JoinHorizontal
	// so each pane keeps its own border.
	bodyHeight := height - 2 - footerLines // title + info + footer
	if bodyHeight < 4 {
		bodyHeight = 4
	}
	listW := splitListPaneWidth(width)
	detailW := width - listW
	if detailW < 20 {
		detailW = 20
	}
	listPane := renderSplitListPane(m, listW, bodyHeight)
	detailPane := renderSplitDetailPane(m, detailW, bodyHeight)
	body := lipgloss.JoinHorizontal(lipgloss.Top, listPane, detailPane)
	return strings.Join([]string{title, body, infoLine, footer}, "\n")
}

// renderSplitListPane renders the bordered list pane: list-table
// body inside a lipgloss panel whose border color reflects the
// focus state. paneW is the OUTER width including the 2-cell border
// surround; paneH is the OUTER height. Inner content takes
// paneW-2 cells wide and paneH-2 rows tall.
func renderSplitListPane(m Model, paneW, paneH int) string {
	innerW := paneW - 2
	innerH := paneH - 2
	if innerW < 10 {
		innerW = 10
	}
	if innerH < 2 {
		innerH = 2
	}
	chrome := m.chrome()
	chrome.narrow = true
	body := m.list.ViewBody(innerW, innerH, chrome)
	return splitPaneStyle(m.focus == focusList, paneW, paneH).Render(body)
}

// renderSplitDetailPane renders the bordered detail pane. When no
// detail is open (initial split-mode boot) we render a placeholder
// hint inviting the user to pick an issue from the list pane;
// otherwise we render dm's body+activity area inside the border.
func renderSplitDetailPane(m Model, paneW, paneH int) string {
	innerW := paneW - 2
	innerH := paneH - 2
	if innerW < 10 {
		innerW = 10
	}
	if innerH < 2 {
		innerH = 2
	}
	body := splitDetailBody(m, innerW, innerH)
	return splitPaneStyle(m.focus == focusDetail, paneW, paneH).Render(body)
}

// splitDetailBody picks the rendered body for the detail pane. With
// an open issue, defer to dm.ViewSplit; otherwise show the empty
// hint so the pane never goes blank.
func splitDetailBody(m Model, innerW, innerH int) string {
	if m.detail.issue == nil {
		return splitDetailEmptyHint(innerW, innerH)
	}
	return m.detail.ViewSplit(innerW, innerH, m.chrome())
}

// splitDetailEmptyHint is the placeholder text shown in the detail
// pane when no issue is open (initial split-mode boot, or after the
// user opened detail then somehow cleared dm.issue). Centered
// vertically + horizontally so the pane reads as intentionally
// empty rather than broken.
func splitDetailEmptyHint(innerW, innerH int) string {
	hint := subtleStyle.Render("select an issue from the list pane")
	if innerW <= 0 || innerH <= 0 {
		return hint
	}
	return lipgloss.Place(innerW, innerH, lipgloss.Center, lipgloss.Center, hint)
}

// splitPaneStyle returns the border style for one pane. The focused
// pane uses panelActiveBorder (magenta); the inactive pane uses
// panelInactiveBorder (gray). Width/Height set the OUTER dimensions
// so callers know how much space the rendered string occupies.
func splitPaneStyle(focused bool, paneW, paneH int) lipgloss.Style {
	border := panelInactiveBorder
	if focused {
		border = panelActiveBorder
	}
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(border).
		Width(paneW - 2).
		Height(paneH - 2)
}

// renderSplitInfoLine renders the shared info line at the bottom of
// the split layout (one row above the footer). Mirrors the per-view
// info line: panel prompt > status > SSE-degraded > toast > scroll
// indicator. Sourced from whichever pane is focused so the right
// pane's contextual hint surfaces.
func renderSplitInfoLine(width int, m Model) string {
	body := splitInfoBody(m)
	return statsLineStyle.Render(padToWidth(body, titleBarInnerWidth(width)))
}

// splitInfoBody picks the info-line text for the split layout from
// the focused pane's state. Panel prompts and command bars own the
// info row when active; otherwise fall through to the focused
// pane's status text or any active toast/SSE hint.
func splitInfoBody(m Model) string {
	chrome := m.chrome()
	if chrome.input.kind.isCommandBar() {
		return renderInfoBar(chrome.input, titleBarInnerWidth(m.width))
	}
	if chrome.input.kind.isPanelPrompt() {
		return renderInfoPrompt(chrome.input, titleBarInnerWidth(m.width))
	}
	if m.focus == focusList && m.list.status != "" {
		return m.list.status
	}
	if m.focus == focusDetail && m.detail.status != "" {
		return m.detail.status
	}
	if chrome.sseStatus != sseConnected {
		return sseDegradedFlash(chrome.sseStatus)
	}
	if chrome.toast != nil {
		return chrome.toast.text
	}
	if m.focus == focusList {
		return rightAlignInside(
			footerPositionIndicator(m.list.cursor, len(m.list.visibleRows())),
			titleBarInnerWidth(m.width),
		)
	}
	if m.focus == focusDetail {
		return rightAlignInside(
			splitDetailScrollIndicator(m), titleBarInnerWidth(m.width),
		)
	}
	return ""
}

// splitDetailScrollIndicator returns the viewport position hint for
// the detail pane in split mode. The pane's inner height (after the
// 2-cell border) is the visible window; the document's total line
// count is computed against the same width the pane renders at.
func splitDetailScrollIndicator(m Model) string {
	if m.detail.issue == nil {
		return ""
	}
	bodyHeight := splitBodyHeight(m)
	listW := splitListPaneWidth(m.width)
	detailW := m.width - listW
	if detailW < 20 {
		detailW = 20
	}
	innerW := detailW - 2
	innerH := bodyHeight - 2
	if innerW < 10 {
		innerW = 10
	}
	if innerH < 2 {
		innerH = 2
	}
	docLines, _ := m.detail.detailDocumentLines(innerW, m.chrome())
	scroll := clampScroll(m.detail.scroll, len(docLines), innerH)
	return documentScrollIndicator(len(docLines), scroll, innerH)
}

// splitBodyHeight mirrors the body-height calculation in renderSplit
// so the indicator math matches what's actually drawn.
func splitBodyHeight(m Model) int {
	footerLines := helpLines(m.splitHelpRows(), m.width)
	bodyHeight := m.height - 2 - footerLines // title + info + footer
	if bodyHeight < 4 {
		bodyHeight = 4
	}
	return bodyHeight
}

// renderSplitFooter renders the shared footer help table for the split
// layout. The help items track the focused pane: list bindings when
// focusList; detail bindings when focusDetail.
func renderSplitFooter(width int, m Model) string {
	return renderFooterHelpTable(m.splitHelpRows(), width)
}

// ViewSplit renders the detail document inside the M6 split-mode
// pane. The shared split frame owns the global title bar and footer
// chrome, so the pane skips the title bar but keeps the same
// gutter + sheet grammar as stacked detail. The pane already has a
// 1-cell border on every side (lipgloss border), so the content
// already sits indented from the surrounding chrome — the gutter
// just adds breathing room inside the panel.
//
// Like the stacked View, the pane is one continuous scrolling
// document: dm.scroll windows the assembled lines and ↑/↓ slide the
// viewport.
func (dm detailModel) ViewSplit(width, height int, chrome viewChrome) string {
	if dm.loading {
		return statusStyle.Render("loading…")
	}
	if dm.issue == nil {
		return statusStyle.Render("no issue selected")
	}
	if width <= 0 || height < 6 {
		return dm.renderTinyFallback(width)
	}
	docLines, _ := dm.detailDocumentLines(width, chrome)
	scroll := clampScroll(dm.scroll, len(docLines), height)
	windowed := windowDocLines(docLines, scroll, height, width)
	return strings.Join(windowed, "\n")
}

// pickHighlightedIssue returns a copy of the issue currently under
// the list cursor in the filtered slice, or false when the list is
// empty. Used by the M6 detail-follows-cursor path to retarget
// dm.issue immediately as the cursor moves.
func pickHighlightedIssue(lm listModel) (Issue, bool) {
	rows := lm.visibleRows()
	if len(rows) == 0 {
		return Issue{}, false
	}
	idx := lm.cursor
	if idx >= len(rows) {
		idx = len(rows) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return rows[idx].issue, true
}
