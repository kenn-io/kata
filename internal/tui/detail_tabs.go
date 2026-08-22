package tui

import (
	"fmt"
	"strings"

	"github.com/mattn/go-runewidth"
)

// tabState carries the per-tab loading / error markers from the model
// to the renderer. A non-nil err takes priority over loading; both
// short-circuit the entry-list path so the user gets a clear hint.
type tabState struct {
	loading bool
	err     error
}

// commentChunks builds the chunk slice for the comments tab. Each
// non-placeholder chunk is one comment: cursor marker + author + dim
// timestamp, followed by indented Markdown comment body and a blank
// separator. Empty / loading / errored states return a single
// placeholder chunk via tabPlaceholder. The chunks ARE the rendered
// content — detailDocumentLines emits them directly without a second
// markdown pass.
func commentChunks(cs []CommentEntry, width, cursor int, ts tabState) []entryChunk {
	if placeholder := tabPlaceholder(ts, "comments", "(no comments)", len(cs)); placeholder != nil {
		return []entryChunk{*placeholder}
	}
	chunks := make([]entryChunk, 0, len(cs))
	authorW := commentAuthorWidth(cs)
	for i, c := range cs {
		author := padToWidth(commentAuthorStyle(c.Author), authorW)
		header := fmt.Sprintf("%s  %s", author, subtleStyle.Render(formatDocumentTime(c.CreatedAt)))
		lines := []string{applyActivityCursor(header, i == cursor)}
		for _, ln := range renderMarkdownLines(c.Body, max(1, width-2)) {
			lines = append(lines, "  "+ln)
		}
		lines = append(lines, "")
		chunks = append(chunks, entryChunk{lines: lines})
	}
	return chunks
}

// eventChunks builds the chunk slice for the events tab. Most events
// produce a single-line chunk:
// "[type] timestamp actor — description". issue.closed events expand
// to additional indented lines surfacing the close message and each
// evidence item so reviewers can audit the close from the events tab
// without dropping to `kata audit closes`.
func eventChunks(es []EventLogEntry, width, cursor int, ts tabState) []entryChunk {
	if placeholder := tabPlaceholder(ts, "events", "(no events yet)", len(es)); placeholder != nil {
		return []entryChunk{*placeholder}
	}
	chunks := make([]entryChunk, 0, len(es))
	for i, e := range es {
		chunks = append(chunks, entryChunk{lines: eventChunkLines(e, width, i == cursor)})
	}
	return chunks
}

// linkChunks builds one single-line chunk per link:
// "[type] → #to ← #from  by author @ timestamp". The "(open|closed)"
// status isn't on the LinkEntry projection; pressing Enter jumps to
// the target. Type is daemon-defined; Author is agent-supplied and is
// sanitized so a malicious link author can't push the terminal around.
// Task 18 owns the final visual shape (kata#abc4 qualified rendering);
// this commit only swaps the wire field names so the package compiles
// against the new LinkEntry shape.
func linkChunks(ls []LinkEntry, width, cursor int, ts tabState) []entryChunk {
	_ = width
	if placeholder := tabPlaceholder(ts, "links", "(no links)", len(ls)); placeholder != nil {
		return []entryChunk{*placeholder}
	}
	chunks := make([]entryChunk, 0, len(ls))
	for i, l := range ls {
		line := fmt.Sprintf("[%s] → #%s ← #%s  by %s @ %s",
			l.Type, l.To.ShortID, l.From.ShortID,
			sanitizeForDisplay(l.Author), fmtTime(l.CreatedAt))
		chunks = append(chunks, entryChunk{lines: []string{
			applyActivityCursor(line, i == cursor),
		}})
	}
	return chunks
}

// renderCommentsTab / renderEventsTab / renderLinksTab assemble the
// chunks into a windowed, height-bounded view. The chunk-builders
// above are also reachable via detailModel.activeChunks for the
// unified detail document; the per-tab renderers stay for tests that
// need windowing behaviour at a fixed height.
func renderCommentsTab(cs []CommentEntry, width, height, cursor int, ts tabState) string {
	chunks := commentChunks(cs, width, cursor, ts)
	if len(chunks) == 0 {
		return ""
	}
	if isPlaceholderChunks(chunks, ts, len(cs)) {
		return assembleTab(nil, chunks, width, height, -1)
	}
	return assembleTab(nil, chunks, width, height, cursor)
}

func renderEventsTab(es []EventLogEntry, width, height, cursor int, ts tabState) string {
	chunks := eventChunks(es, width, cursor, ts)
	if len(chunks) == 0 {
		return ""
	}
	if isPlaceholderChunks(chunks, ts, len(es)) {
		return assembleTab(nil, chunks, width, height, -1)
	}
	return assembleTab(nil, chunks, width, height, cursor)
}

func renderLinksTab(ls []LinkEntry, width, height, cursor int, ts tabState) string {
	chunks := linkChunks(ls, width, cursor, ts)
	if len(chunks) == 0 {
		return ""
	}
	if isPlaceholderChunks(chunks, ts, len(ls)) {
		return assembleTab(nil, chunks, width, height, -1)
	}
	return assembleTab(nil, chunks, width, height, cursor)
}

// isPlaceholderChunks reports whether the chunks slice represents a
// loading / errored / empty placeholder rather than rendered entries.
// The per-tab renderers pass cursor=-1 to assembleTab in that case so
// windowChunkBounds doesn't try to anchor on a fake "row 0".
func isPlaceholderChunks(chunks []entryChunk, ts tabState, n int) bool {
	if len(chunks) != 1 {
		return false
	}
	return ts.err != nil || ts.loading || n == 0
}

// tabPlaceholder returns the chunk to render in lieu of the entry list
// when the tab is loading, errored, or empty. Returns nil when the
// caller should render the entries normally.
func tabPlaceholder(ts tabState, tab, emptyHint string, n int) *entryChunk {
	if ts.err != nil {
		return &entryChunk{lines: []string{
			errorStyle.Render(tab + ": " + ts.err.Error()),
		}}
	}
	if ts.loading {
		return &entryChunk{lines: []string{statusStyle.Render("(loading…)")}}
	}
	if n == 0 {
		return &entryChunk{lines: []string{statusStyle.Render(emptyHint)}}
	}
	return nil
}

// entryChunk groups the lines that belong to one tab entry. Comments
// produce multi-line chunks (header + wrapped body + separator);
// events and links produce one-line chunks. Windowing operates on
// chunk granularity so a cursor at entry N never lands on a partial
// row.
type entryChunk struct {
	lines []string
}

func commentAuthorStyle(author string) string {
	return titleStyle.Render(sanitizeForDisplay(author))
}

func commentAuthorWidth(cs []CommentEntry) int {
	width := 0
	for _, c := range cs {
		if w := runewidth.StringWidth(sanitizeForDisplay(c.Author)); w > width {
			width = w
		}
	}
	return min(width, 16)
}

// applyActivityCursor prefixes activity rows with a text cursor marker
// instead of painting the row. This keeps the document body free of
// filled blocks and remains readable under NO_COLOR.
func applyActivityCursor(line string, isCursor bool) string {
	if isCursor {
		return "> " + line
	}
	return "  " + line
}

// assembleTab joins the header lines with the windowed entry chunks
// and clips the result to width. cursor is the entry index of the
// active row (or -1 for empty-tab placeholders).
func assembleTab(
	headers []string, chunks []entryChunk, width, height, cursor int,
) string {
	avail := max(height-len(headers), 1)
	windowed := windowChunks(chunks, cursor, avail)
	out := make([]string, 0, len(headers)+8)
	out = append(out, headers...)
	for _, ch := range windowed {
		out = append(out, ch.lines...)
	}
	return clipTab(out, width, height)
}

// windowChunks returns the contiguous slice of chunks that includes
// the cursor entry and fits within budget lines. When everything fits,
// the input is returned unchanged. See windowChunkBounds for the
// indices-only variant the scroll indicator uses.
func windowChunks(chunks []entryChunk, cursor, budget int) []entryChunk {
	start, end := windowChunkBounds(chunks, cursor, budget)
	return chunks[start:end]
}

// windowChunkBounds returns the [start, end) entry indices of the
// chunk window that fits within budget lines around the cursor. When
// everything fits, returns [0, n). When it doesn't, the window slides
// so the cursor's chunk is fully visible — preferring to anchor at
// the top until the cursor crosses the budget, then scrolling so the
// cursor sits near the bottom of the viewport. The cursor's own
// chunk is always included even if it alone exceeds the budget —
// preferable to hiding the cursor entirely.
//
// chunks with zero lines (defensive — empty placeholders) are still
// kept so windowing arithmetic doesn't drift.
//
// Extracted from windowChunks so the detail scroll indicator can ask
// "how many entries actually fit?" in entry units (matching the
// renderer's view) instead of comparing entry count to line budget
// directly — see roborev #119 finding 2.
func windowChunkBounds(chunks []entryChunk, cursor, budget int) (int, int) {
	if chunks == nil {
		return 0, 0
	}
	n := len(chunks)
	if n == 0 {
		return 0, 0
	}
	if budget <= 0 {
		return 0, n
	}
	if totalLines(chunks) <= budget {
		return 0, n
	}
	c := cursor
	if c < 0 || c >= n {
		c = 0
	}
	// gosec G602 cannot see that c was clamped to [0, n) above.
	used := len(chunks[c].lines) //nolint:gosec // c was clamped to [0,n)
	start, end := c, c+1
	for start > 0 {
		add := len(chunks[start-1].lines)
		if used+add > budget {
			break
		}
		start--
		used += add
	}
	for end < n {
		add := len(chunks[end].lines)
		if used+add > budget {
			break
		}
		used += add
		end++
	}
	return start, end
}

// totalLines sums the line counts across every chunk.
func totalLines(chunks []entryChunk) int {
	n := 0
	for _, ch := range chunks {
		n += len(ch.lines)
	}
	return n
}

// clipTab truncates lines to width and caps the slice at height. Empty
// input or zero height is an empty render so the layout doesn't shift.
func clipTab(lines []string, width, height int) string {
	if height < 1 {
		return ""
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		out = append(out, truncate(ln, width))
	}
	return strings.Join(out, "\n")
}
