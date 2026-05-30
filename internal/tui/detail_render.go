package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// View renders the stacked detail view as a single scrolling document.
// The layout is:
//
//   - line 1: full-width project/title bar (chrome, never scrolls)
//   - line 2: blank breather under the chrome (chrome, never scrolls)
//   - lines 3..H-2: detail document — issue header + body + (optional
//     children) + (optional activity), windowed by dm.scroll
//   - line H-1: info line (chrome row, viewport indicator + status)
//   - line H:   footer help table (chrome row)
//
// Body, children, and activity render in full inside the document; the
// per-section row budgets that used to split the available height are
// gone. ↑/↓ scrolls the document one line; PgUp/PgDn pages by the
// visible window.
//
// Content lives inside a 96-cell maximum measure. Spare terminal width
// to the right is left blank — the redesign avoids stretching section
// bands across wide terminals so the page reads as a focused document,
// not a half-empty workspace.
func (dm detailModel) View(width, height int, chrome viewChrome) string {
	if dm.loading {
		return statusStyle.Render("loading…")
	}
	if dm.issue == nil {
		return statusStyle.Render("no issue selected")
	}
	if width <= 0 || height < listMinHeight {
		return dm.renderTinyFallback(width)
	}
	helpRows := detailHelpRows(dm, chrome)
	footerLines := helpLines(helpRows, width)
	footer := renderFooterHelpTable(helpRows, width)
	titleBar := renderTitleBar(width, chrome.scope, chrome.version, chrome.daemon)
	// Top chrome (title + blank) + bottom chrome (info + footer) frame
	// the viewport. Whatever rows are left belong to the document.
	visible := height - 2 - 1 - footerLines
	if visible < 1 {
		visible = 1
	}
	docLines, _ := dm.detailDocumentLines(width, chrome)
	scroll := clampScroll(dm.scroll, len(docLines), visible)
	windowed := windowDocLines(docLines, scroll, visible, width)
	infoLine := dm.renderInfoLine(width, chrome, len(docLines), scroll, visible)
	parts := append([]string{titleBar, ""}, windowed...)
	parts = append(parts, infoLine, footer)
	return strings.Join(parts, "\n")
}

// renderTinyFallback is the degraded render for terminals below the
// minimum height. Window the document with whatever space is left so
// the user still sees something instead of an error or blank screen.
func (dm detailModel) renderTinyFallback(width int) string {
	if width <= 0 {
		width = 1
	}
	docLines, _ := dm.detailDocumentLines(width, viewChrome{})
	scroll := clampScroll(dm.scroll, len(docLines), detailMinBodyRows)
	return strings.Join(windowDocLines(docLines, scroll, detailMinBodyRows, width), "\n")
}

// clampScroll keeps dm.scroll within [0, viewportMaxStart) for the
// given document length and viewport size. View / ViewSplit always
// pass the user's intended scroll through this so an off-the-end
// dm.scroll (e.g. after a tab switch trims the document) doesn't
// produce a window of blanks.
func clampScroll(scroll, total, visible int) int {
	if scroll < 0 {
		return 0
	}
	maxStart := viewportMaxStart(total, visible)
	if scroll > maxStart {
		return maxStart
	}
	return scroll
}

// windowDocLines slices [scroll, scroll+visible) of the document and
// pads with blank rows when the document is shorter than the viewport
// so the chrome below stays anchored on the same screen line. Lines
// are emitted as-is (already gutter-prefixed during assembly).
func windowDocLines(lines []string, scroll, visible, width int) []string {
	out := make([]string, 0, visible)
	end := scroll + visible
	if end > len(lines) {
		end = len(lines)
	}
	for i := scroll; i < end; i++ {
		out = append(out, padToWidth(lines[i], width))
	}
	for len(out) < visible {
		out = append(out, blankLine(width))
	}
	return out
}

// documentHeader builds the issue-lead block: title row, byline,
// and the compact metadata strip. Each line is gutter-prefixed so
// the sheet reads as one indented column under the project bar.
func (dm detailModel) documentHeader(width int, chrome viewChrome) []string {
	iss := *dm.issue
	lines := []string{withGutter(renderDocumentTitleStatus(width, iss))}
	if byline := renderDocumentByline(width, iss); byline != "" {
		lines = append(lines, withGutter(byline))
	}
	for _, row := range renderDocumentMetadata(
		width, iss, dm.parent, dm.children, chrome.scope, dm.uidFormat,
	) {
		lines = append(lines, withGutter(row))
	}
	return lines
}

func renderDocumentTitleStatus(width int, iss Issue) string {
	status := renderStatusPill(iss)
	statusPlain := stripANSI(status)
	statusW := runewidth.StringWidth(statusPlain)
	prefix := subtleStyle.Render(fmt.Sprintf("#%s", iss.ShortID))
	prefixW := runewidth.StringWidth(stripANSI(prefix)) + 2
	titleBudget := width - prefixW - statusW - 2
	if titleBudget < 1 {
		titleBudget = 1
	}
	title := prefix + "  " + titleStyle.Render(truncate(sanitizeForDisplay(iss.Title), titleBudget))
	return padLeftRightInside(title, status, width)
}

func renderStatusPill(iss Issue) string {
	text := "[open]"
	style := openStyle
	if iss.DeletedAt != nil {
		text = "[deleted]"
		style = deletedStyle
	} else if iss.Status == "closed" {
		text = "[closed]"
		style = closedStyle
	}
	return style.Render(text)
}

func renderDocumentByline(width int, iss Issue) string {
	parts := documentBylineParts(iss, true, true)
	line := strings.Join(parts, " · ")
	if line == "" {
		return ""
	}
	if runewidth.StringWidth(line) > width {
		parts = documentBylineParts(iss, false, true)
		line = strings.Join(parts, " · ")
	}
	if runewidth.StringWidth(line) > width {
		parts = documentBylineParts(iss, false, false)
		line = strings.Join(parts, " · ")
	}
	return subtleStyle.Render(truncate(line, width))
}

func documentBylineParts(iss Issue, includeCreated, includeAuthor bool) []string {
	parts := []string{}
	if includeAuthor && iss.Author != "" {
		parts = append(parts, "authored by "+sanitizeForDisplay(iss.Author))
	}
	if includeCreated && !iss.CreatedAt.IsZero() {
		parts = append(parts, "created "+formatDocumentTime(iss.CreatedAt))
	}
	if !iss.UpdatedAt.IsZero() {
		parts = append(parts, "updated "+humanizeRelative(iss.UpdatedAt))
	}
	return parts
}

func formatDocumentTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("Jan 2 15:04")
}

// renderDocumentMetadata returns the compact metadata strip that
// sits below the byline. Owner and parent are always shown — the
// "none" placeholder is informative because both fields are used as
// triage signals. Labels and children are omitted entirely when
// empty so the page does not carry rows like `labels: none` whose
// only payload is a placeholder.
//
// In all-projects scope the project name leads on its own row so
// the user can tell which project a cross-project result lives in
// without consulting the global title bar (which reads "all" in
// that scope).
func renderDocumentMetadata(
	width int, iss Issue, parent *IssueRef, children []Issue, sc scope, uidFormat uidDisplayFormat,
) []string {
	rows := []string{}
	if sc.allProjects {
		rows = append(rows, "project: "+sanitizeForDisplay(sc.projectName))
	}
	owner := metadataLabel("owner:") + " " + ownerDocumentText(iss.Owner)
	parentText := metadataLabel("parent:") + " " + parentDocumentText(parent)
	rows = append(rows, joinMetadataRow(owner, parentText, width)...)
	priorityText := metadataLabel("priority:") + " " + priorityDocumentText(iss.Priority)
	rows = append(rows, truncate(priorityText, width))
	if len(iss.Labels) > 0 {
		labelsBudget := width - runewidth.StringWidth("labels: ")
		labels := metadataLabel("labels:") + " " + labelsDocumentText(iss.Labels, labelsBudget)
		rows = append(rows, truncate(labels, width))
	}
	if len(children) > 0 {
		count := metadataLabel("children:") + " " + childrenCountSummary(children)
		rows = append(rows, truncate(count, width))
	}
	if uid := formatIssueUID(iss.UID, uidFormat); uid != "" {
		rows = append(rows, truncate(metadataLabel("uid:")+" "+uid, width))
	}
	return rows
}

func parseUIDDisplayFormat(v string) uidDisplayFormat {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "short":
		return uidDisplayShort
	case "full":
		return uidDisplayFull
	default:
		return uidDisplayNone
	}
}

func formatIssueUID(uid string, format uidDisplayFormat) string {
	if uid == "" {
		return ""
	}
	switch format {
	case uidDisplayShort:
		if len(uid) <= 8 {
			return "~" + uid
		}
		return "~" + uid[:8]
	case uidDisplayFull:
		return uid
	default:
		return ""
	}
}

// joinMetadataRow returns the (owner, parent) pair as one inline row
// when both fit the sheet width, or as two stacked rows when the
// combined width would overflow. Stacking — rather than truncating —
// preserves the parent reference text on narrow terminals where the
// title would otherwise be lost to ellipsis.
func joinMetadataRow(left, right string, width int) []string {
	lw := runewidth.StringWidth(stripANSI(left))
	rw := runewidth.StringWidth(stripANSI(right))
	if lw+3+rw <= width {
		return []string{left + "   " + right}
	}
	return []string{truncate(left, width), truncate(right, width)}
}

func metadataLabel(label string) string {
	return subtleStyle.Render(label)
}

func ownerDocumentText(owner *string) string {
	if owner == nil || *owner == "" {
		return "none"
	}
	return sanitizeForDisplay(*owner)
}

func labelsDocumentText(labels []string, available int) string {
	if len(labels) == 0 {
		return "none"
	}
	if available < 1 {
		available = 1
	}
	return renderLabelChips(labels, available)
}

func parentDocumentText(parent *IssueRef) string {
	if parent == nil {
		return "none"
	}
	return fmt.Sprintf("#%s %s", parent.ShortID, sanitizeForDisplay(parent.Title))
}

// priorityDocumentText renders the priority value for the detail
// metadata row. Pn for a set value (n in 0..4, where 0 is highest
// priority); "none" when unset.
func priorityDocumentText(p *int64) string {
	if p == nil {
		return "none"
	}
	return fmt.Sprintf("P%d", *p)
}

// renderDocumentSectionHeader renders a section label (Body /
// Children / Activity). The label sits inside the gutter without a
// background slab — the redesign relies on bold weight + position
// rather than a chrome band so the page reads as a quiet document.
func renderDocumentSectionHeader(label string) string {
	return withGutter(detailSectionHeaderStyle.Render(label))
}

// renderActivityHeader is a section header with the three tab chips
// trailing the label. Active tab is wrapped in `[ ]` brackets so the
// active state survives `KATA_COLOR_MODE=none`. The full strip is
// truncated to `width` cells so a narrow terminal never produces an
// overflow row that wraps under the gutter.
func (dm detailModel) renderActivityHeader(width int) string {
	tabs := [detailTabCount]string{
		fmt.Sprintf("Comments (%d)", len(dm.comments)),
		fmt.Sprintf("Events (%d)", len(dm.events)),
		fmt.Sprintf("Links (%d)", len(dm.links)),
	}
	parts := []string{detailSectionHeaderStyle.Render("Activity")}
	for i, tab := range tabs {
		if detailTab(i) == dm.activeTab {
			parts = append(parts, tabActive.Render("[ "+tab+" ]"))
		} else {
			parts = append(parts, tabInactive.Render(tab))
		}
	}
	return withGutter(truncate(strings.Join(parts, "   "), width))
}

// documentGutter is the left-side cell padding for content rows in
// the stacked detail view. The gutter visually separates the document
// content from the global title/info chrome at the page edges.
const documentGutter = 2

// documentSheetMaxWidth caps the readable measure of issue content
// rows. Wider terminals leave the spare cells blank rather than
// stretching backgrounds across the screen.
const documentSheetMaxWidth = 96

// documentSheetWidth returns the per-row content budget — the cells
// available for issue content after the gutter is reserved, capped at
// documentSheetMaxWidth so wide terminals do not blow out paragraph
// measure.
func documentSheetWidth(termWidth int) int {
	w := termWidth - documentGutter
	if w > documentSheetMaxWidth {
		w = documentSheetMaxWidth
	}
	if w < 1 {
		w = 1
	}
	return w
}

// withGutter prefixes each newline-separated line in s with the
// document gutter. Empty input returns the gutter alone so blank
// rows still carry the indent and the page reads as a uniform
// column.
func withGutter(s string) string {
	if s == "" {
		return strings.Repeat(" ", documentGutter)
	}
	gutter := strings.Repeat(" ", documentGutter)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = gutter + line
	}
	return strings.Join(lines, "\n")
}

func blankLine(width int) string {
	return normalRowStyle.Render(strings.Repeat(" ", max(0, width)))
}

// renderInfoLine renders the info line just above the footer for the
// detail view. Priority order: active panel prompt > flash >
// SSE-degraded > toast > viewport scroll indicator. Always rendered
// inside statsLineStyle so the row reads as chrome even when blank.
//
// total is the document line count and scroll/visible are the current
// window. When the document fits the viewport, the indicator is
// suppressed.
func (dm detailModel) renderInfoLine(width int, chrome viewChrome, total, scroll, visible int) string {
	body := ""
	switch {
	case chrome.input.kind.isPanelPrompt():
		body = renderInfoPrompt(chrome.input, titleBarInnerWidth(width))
	case dm.status != "":
		body = dm.status
	case chrome.sseStatus != sseConnected:
		body = sseDegradedFlash(chrome.sseStatus)
	case chrome.toast != nil:
		body = chrome.toast.text
	default:
		if indicator := documentScrollIndicator(total, scroll, visible); indicator != "" {
			body = rightAlignInside(indicator, titleBarInnerWidth(width))
		}
	}
	return statsLineStyle.Render(padToWidth(body, titleBarInnerWidth(width)))
}

// documentScrollIndicator returns the "[lines X-Y of Z]" hint for the
// detail viewport, or empty string when the document fits the viewport.
// Lines are 1-indexed for display.
func documentScrollIndicator(total, scroll, visible int) string {
	if total <= visible || visible <= 0 || total <= 0 {
		return ""
	}
	start := scroll + 1
	end := scroll + visible
	if end > total {
		end = total
	}
	return fmt.Sprintf("[lines %d-%d of %d]", start, end, total)
}

// renderInfoPrompt renders an active panel-local prompt as a single
// info-line row. Bordered/labeled at panel-prompt scope makes the
// info line too tall; instead the prompt's title prefixes the buffer.
//
// The textinput's View() carries bubbles' own cursor-paint ANSI;
// keep it intact (don't sanitize — strips the cursor) and width-clip
// with ansi.Truncate so escape sequences survive.
func renderInfoPrompt(s inputState, innerWidth int) string {
	field := s.activeField()
	if field == nil {
		return ansi.Truncate(s.title+": ", innerWidth, "…")
	}
	body := s.title + ": " + field.input.View()
	return ansi.Truncate(body, innerWidth, "…")
}

// detailMinBodyRows / detailMinTabRows are the floors so neither
// pane collapses to zero on small terminals.
const (
	detailMinBodyRows = 4
	detailMinTabRows  = 3
)

// detailAnchors records the document row index where each section
// begins, plus the document row of the section cursor. Tab and j/k
// use these to scroll the unified viewport so the focused section /
// selected row is visible. -1 means the section or cursor is absent.
type detailAnchors struct {
	body        int
	children    int
	activity    int
	childCursor int
	tabCursor   int
	total       int
}

// emptyAnchors returns a detailAnchors with all section / cursor rows
// marked absent. Callers fill in only the rows that actually exist
// for the current detail model.
func emptyAnchors() detailAnchors {
	return detailAnchors{
		body:        -1,
		children:    -1,
		activity:    -1,
		childCursor: -1,
		tabCursor:   -1,
	}
}

// detailDocumentLines flattens the entire detail content (issue header,
// body, children, activity) into one slice of rendered rows. The slice
// excludes the global title bar and the bottom info+footer chrome —
// those are owned by the caller (View / ViewSplit) and stay pinned.
//
// anchors reports where each section starts in the slice (and where
// the section cursor sits), so handleNavKey can scroll the viewport
// to bring focus into view when Tab cycles or j/k advances.
func (dm detailModel) detailDocumentLines(width int, chrome viewChrome) ([]string, detailAnchors) {
	sheetWidth := documentSheetWidth(width)
	lines := make([]string, 0, 32)
	anchors := emptyAnchors()
	addBlock := func(block string) {
		lines = append(lines, strings.Split(block, "\n")...)
	}

	for _, hdr := range dm.documentHeader(sheetWidth, chrome) {
		addBlock(hdr)
	}

	lines = append(lines, "", renderDocumentSectionHeader("Body"))
	anchors.body = len(lines)
	addBlock(withGutter(dm.renderBodyFull(sheetWidth)))

	if len(dm.children) > 0 {
		lines = append(lines, "", renderDocumentSectionHeader("Children"))
		anchors.children = len(lines)
		cursor := clampInt(dm.childCursor, 0, len(dm.children)-1)
		anchors.childCursor = anchors.children + cursor
		addBlock(withGutter(dm.renderChildrenFull(sheetWidth)))
	}

	if dm.hasActivity() {
		lines = append(lines, "", dm.renderActivityHeader(sheetWidth))
		anchors.activity = len(lines)
		chunks := dm.activeChunks(sheetWidth)
		// tabCursor anchor only makes sense when the chunks map to real
		// rows; loading / errored / empty placeholders return a single
		// pseudo-chunk with no cursor target.
		if dm.activeRowCount() > 0 && len(chunks) > 0 {
			cursor := clampInt(dm.tabCursor, 0, len(chunks)-1)
			offset := 0
			for i := 0; i < cursor; i++ {
				offset += len(chunks[i].lines)
			}
			anchors.tabCursor = anchors.activity + offset
		}
		for _, ch := range chunks {
			for _, line := range ch.lines {
				lines = append(lines, withGutter(truncate(line, sheetWidth)))
			}
		}
	}

	anchors.total = len(lines)
	return lines, anchors
}

// viewportMaxStart returns the largest dm.scroll value that still
// produces a fully-utilized viewport for a document of `total` rows
// with `visible` rendered rows. When the document fits the viewport,
// returns 0. Callers clamp dm.scroll against this.
func viewportMaxStart(total, visible int) int {
	if total <= visible || visible <= 0 {
		return 0
	}
	return total - visible
}

// scrollToReveal returns the new scroll offset that keeps `row` inside
// a `visible`-row window starting at `scroll`. If the row is above the
// window, the window slides up to land the row at the top; below the
// window, it slides down so the row is the last visible line.
//
// total bounds the result so an off-the-end anchor never lets scroll
// run past viewportMaxStart.
func scrollToReveal(scroll, row, visible, total int) int {
	if row < 0 || visible <= 0 {
		return scroll
	}
	maxStart := viewportMaxStart(total, visible)
	if row < scroll {
		scroll = row
	} else if row >= scroll+visible {
		scroll = row - visible + 1
	}
	if scroll < 0 {
		scroll = 0
	}
	if scroll > maxStart {
		scroll = maxStart
	}
	return scroll
}

func renderHierarchySummary(width int, parent *IssueRef, children []Issue) string {
	left := "Parent: -"
	if parent != nil {
		left = fmt.Sprintf("Parent: #%s %s", parent.ShortID, sanitizeForDisplay(parent.Title))
	}
	right := "Children: " + childrenCountSummary(children)
	rightW := runewidth.StringWidth(right)
	leftBudget := width - rightW - 1
	if leftBudget < 1 {
		leftBudget = 1
	}
	left = truncate(left, leftBudget)
	return padLeftRightInside(left, right, width)
}

func childrenCountSummary(children []Issue) string {
	open := 0
	for _, child := range children {
		if child.Status == "open" {
			open++
		}
	}
	return fmt.Sprintf("%d open / %d total", open, len(children))
}

// renderBodyFull returns every wrapped body line as one joined string.
// Hard-wrap (truncate) keeps v1 simple; soft word-wrap is deferred.
// Body is sanitized before wrapping so agent-authored ANSI / control
// sequences cannot reach the terminal. The unified detail-document
// scroll (dm.scroll) windows this at the document level — the per-
// section windowing that used to live here was removed when the body /
// children / activity sections were merged into one scrollable column.
func (dm detailModel) renderBodyFull(width int) string {
	wrapped := renderMarkdownLines(dm.issue.Body, width)
	if len(wrapped) == 0 {
		return statusStyle.Render("(no description)")
	}
	return strings.Join(wrapped, "\n")
}

// renderChildrenFull returns every child row as one joined string with
// the cursor highlight applied to the selected child when focus is on
// the children section. Document-level scroll (dm.scroll) handles
// visibility — there is no per-section row budget anymore.
func (dm detailModel) renderChildrenFull(width int) string {
	if len(dm.children) == 0 {
		return ""
	}
	cursor := clampInt(dm.childCursor, 0, len(dm.children)-1)
	lines := make([]string, 0, len(dm.children))
	for i, child := range dm.children {
		selected := i == cursor && dm.detailFocus == focusChildren
		line := renderChildIssueRow(child, selected, width)
		switch {
		case selected:
			line = selectedStyle.Render(padToWidth(line, width))
		case i%2 == 1:
			line = altRowStyle.Render(padToWidth(line, width))
		default:
			line = normalRowStyle.Render(padToWidth(line, width))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func renderChildIssueRow(child Issue, selected bool, width int) string {
	const (
		markerW = 2
		numW    = 7
		statusW = 10
		ownerW  = 12
		updateW = 10
	)
	titleW := width - markerW - numW - statusW - ownerW - updateW
	if titleW < 12 {
		titleW = 12
	}
	marker := " "
	if selected {
		marker = ">"
	}
	parts := []string{
		padToWidth(marker, markerW),
		padToWidth(fmt.Sprintf("#%s", child.ShortID), numW),
		padToWidth(statusChip(child), statusW),
		padToWidth(truncate(sanitizeForDisplay(child.Title), titleW), titleW),
		padToWidth(truncate(sanitizeForDisplay(ownerText(child.Owner)), ownerW-1), ownerW),
		padToWidth(humanizeRelative(child.UpdatedAt), updateW),
	}
	return strings.Join(parts, "")
}

// wrapBody splits s on newlines, then hard-wraps each segment to width.
func wrapBody(s string, width int) []string {
	if s == "" {
		return nil
	}
	if width < 1 {
		width = 1
	}
	out := []string{}
	for _, raw := range strings.Split(s, "\n") {
		if raw == "" {
			out = append(out, "")
			continue
		}
		out = append(out, hardWrap(raw, width)...)
	}
	return out
}

// hardWrap breaks s into chunks no wider than width cells.
func hardWrap(s string, width int) []string {
	out := []string{}
	for runewidth.StringWidth(s) > width {
		head := runewidth.Truncate(s, width, "")
		if head == "" {
			_, sz := utf8.DecodeRuneInString(s)
			out = append(out, s[:sz])
			s = s[sz:]
			continue
		}
		out = append(out, head)
		s = s[len(head):]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func (dm detailModel) hasActivity() bool {
	return len(dm.comments) > 0 || len(dm.events) > 0 || len(dm.links) > 0 ||
		dm.commentsLoading || dm.eventsLoading || dm.linksLoading ||
		dm.commentsErr != nil || dm.eventsErr != nil || dm.linksErr != nil
}
