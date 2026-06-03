package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// renderProjects draws the projects-view body: a 5-column table
// (Project / Open / Closed / Total / Updated), an All-projects sentinel
// pinned at row 0, and a 1-line footer for the highlighted row. Spec §5.
// Rows are clipped to the available height with
// cursor-relative scrolling so the chrome (footer + key-hint) stays on
// screen even with many projects on a short terminal.
func renderProjects(m Model) string {
	rows := projectsRows(m.projectsByID, m.projectIdentByID, m.projectStats)
	cursor := m.projectsCursor
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	if cursor < 0 {
		cursor = 0
	}
	helpRows := projectsHelpRows()
	footerLines := helpLines(helpRows, m.width)

	rowBudget := len(rows)
	if m.height > 0 {
		rowBudget = m.height - projectsViewChromeRows(footerLines)
		if rowBudget < 1 {
			rowBudget = 1
		}
	}
	visible := clipProjectsRows(rows, cursor, rowBudget)

	headerCells := []string{"Project", "Open", "Closed", "Total", "Updated"}
	body := []string{
		titleStyle.Render("kata / projects"),
		subtleStyle.Render(fmt.Sprintf("%d projects", len(rows)-1)),
		"",
		renderProjectsHeader(headerCells, m.width),
	}
	for _, vr := range visible {
		body = append(body, renderProjectsRow(vr.row, vr.index == cursor, m.width))
	}
	body = append(body, "")
	if cursor >= 0 && cursor < len(rows) {
		body = append(body, subtleStyle.Render(footerForRow(rows[cursor], m.width)))
	}
	body = append(body, "")
	body = append(body, renderFooterHelpTable(helpRows, m.width))

	return strings.Join(body, "\n")
}

func projectsViewChromeRows(footerLines int) int {
	return 7 + footerLines
}

func projectsHelpRows() [][]helpItem {
	return [][]helpItem{{
		{key: "↑↓", desc: "move"},
		{key: "↵", desc: "open"},
		{key: "esc", desc: "back"},
		{key: "r", desc: "refresh"},
		{key: "D", desc: "daemons"},
		{key: "F", desc: "federation"},
		{key: "?", desc: "help"},
		{key: "q", desc: "quit"},
	}}
}

// projectsVisibleRow pairs a row with its index in the unclipped
// projectsRows slice so the renderer can compare against the (clamped)
// cursor without re-deriving offsets.
type projectsVisibleRow struct {
	row   projectsRow
	index int
}

// clipProjectsRows returns the slice of rows that should render in a
// budget of `budget` lines. The sentinel (rows[0]) is always shown at
// the top — it's the "All projects" anchor users expect to see whenever
// they're in viewProjects. Concrete rows scroll cursor-relative below
// the sentinel.
//
// Special cases:
//   - budget <= 0 or empty rows: return nil (caller should still render
//     the chrome).
//   - budget == 1: only the sentinel renders.
//   - all concrete rows fit: render every row in order.
//
// The scrolling window for concrete rows uses a third-from-top anchor
// matching the list view's windowBounds — more upcoming rows than
// scrolled-past, which matches the common vim/less feel.
func clipProjectsRows(rows []projectsRow, cursor, budget int) []projectsVisibleRow {
	if budget <= 0 || len(rows) == 0 {
		return nil
	}
	out := []projectsVisibleRow{{row: rows[0], index: 0}}
	if budget == 1 || len(rows) == 1 {
		return out
	}
	concreteSlots := budget - 1
	concreteCount := len(rows) - 1
	if concreteCount <= concreteSlots {
		for i := 1; i < len(rows); i++ {
			out = append(out, projectsVisibleRow{row: rows[i], index: i})
		}
		return out
	}
	// Translate cursor to concrete-row index space (cursor==0 is the
	// sentinel; clamp to the concrete window's top in that case so the
	// user sees the head of the list).
	concreteCursor := cursor - 1
	if concreteCursor < 0 {
		concreteCursor = 0
	}
	start, end := windowBounds(concreteCount, concreteCursor, concreteSlots)
	for i := start; i < end; i++ {
		out = append(out, projectsVisibleRow{row: rows[i+1], index: i + 1})
	}
	return out
}

func renderProjectsHeader(cells []string, width int) string {
	// Fixed-width numeric columns; flexible Project column.
	return projectsRowLayout(cells[0], cells[1], cells[2], cells[3], cells[4], width, false)
}

func renderProjectsRow(r projectsRow, highlight bool, width int) string {
	name := r.name
	if r.sentinel {
		name = "All projects"
	} else {
		name = sanitizeForDisplay(name)
	}
	open := fmt.Sprintf("%d", r.stats.Open)
	closed := fmt.Sprintf("%d", r.stats.Closed)
	total := fmt.Sprintf("%d", r.stats.Open+r.stats.Closed)
	updated := updatedColumn(r.stats.LastEventAt)
	return projectsRowLayout(name, open, closed, total, updated, width, highlight)
}

// projectsRowLayout lays out the five columns with the Project column
// flexing and the four numeric/time columns fixed-width and right- or
// left-aligned per spec §5.2.
func projectsRowLayout(project, open, closed, total, updated string, width int, highlight bool) string {
	const (
		openW    = 6
		closedW  = 7
		totalW   = 6
		updatedW = 12
		gap      = 2
	)
	projectW := width - (openW + closedW + totalW + updatedW + 4*gap) - 2
	if projectW < 8 {
		projectW = 8
	}
	cursor := "  "
	if highlight {
		cursor = "▶ "
	}
	line := cursor + padToWidth(project, projectW) +
		strings.Repeat(" ", gap) + padL(open, openW) +
		strings.Repeat(" ", gap) + padL(closed, closedW) +
		strings.Repeat(" ", gap) + padL(total, totalW) +
		strings.Repeat(" ", gap) + padToWidth(updated, updatedW)
	if highlight {
		line = lipgloss.NewStyle().Bold(true).Render(line)
	}
	return line
}

// padL right-aligns s in a width of `w` cells. Counterpart to
// padToWidth (which left-aligns); used for numeric columns where
// the eye scans down right-aligned digits. ANSI-aware on both the
// measure and the truncate paths.
func padL(s string, w int) string {
	width := runewidth.StringWidth(stripANSI(s))
	if width == w {
		return s
	}
	if width > w {
		return ansi.Truncate(s, w, "…")
	}
	return strings.Repeat(" ", w-width) + s
}

// updatedColumn renders the Updated cell. nil (project with zero
// events) is the em-dash per spec §6.1; otherwise we delegate to
// humanizeRelative so the per-bucket format ("30s ago" / "2h ago" /
// "3d ago" / "1w ago") and renderNow injection match the rest of
// the TUI.
func updatedColumn(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return humanizeRelative(*t)
}

// footerForRow renders the 1-line footer for a highlighted row.
func footerForRow(r projectsRow, width int) string {
	if r.sentinel {
		return "issue queue across every registered project"
	}
	label := "project: " + sanitizeForDisplay(r.name)
	if width > 0 && runewidth.StringWidth(label) > width-2 {
		label = runewidth.Truncate(label, width-2, "…")
	}
	return label
}
