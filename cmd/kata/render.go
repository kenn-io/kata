package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"go.kenn.io/kata/internal/textsafe"
)

// issueRow is the render-ready projection of one issue line. Callers
// map their wire structs into this; the renderer sanitizes all
// strings itself via textsafe.Line so a hostile title/owner can't
// break row layout or inject terminal control sequences.
type issueRow struct {
	ID       string // short_id or qualified id
	Title    string
	Owner    string   // already fallback-resolved ("unowned" / "-")
	Priority *int64   // nil = no chip
	Status   string   // "open" | "closed"
	Blocked  bool     // wins over Status for the glyph when Status=="open"
	Labels   []string // renderer picks out epic/bug chips
}

// footerRuleWidth is the width in cells of the "─" rule printed
// between the row list and the summary line.
const footerRuleWidth = 50

// rowRenderer holds lipgloss styles plus the color profile detected
// for the output stream (TTY, NO_COLOR, forced profile in tests).
// Lip Gloss v2 has no per-stream renderer: styles render at full
// fidelity and the profile-aware writer downsamples on write.
type rowRenderer struct {
	profile           colorprofile.Profile
	idStyle           lipgloss.Style
	blockedGlyphStyle lipgloss.Style
	closedGlyphStyle  lipgloss.Style
	p0Style           lipgloss.Style
	p1Style           lipgloss.Style
	p2Style           lipgloss.Style
	p4Style           lipgloss.Style
	epicChipStyle     lipgloss.Style
	bugChipStyle      lipgloss.Style
	ownerStyle        lipgloss.Style
	ruleStyle         lipgloss.Style
	legendStyle       lipgloss.Style
}

// newRowRenderer builds a rowRenderer for w, detecting TTY /
// NO_COLOR / color-profile capability from w and the environment (so
// piping to a file or a NO_COLOR environment degrades to plain text).
func newRowRenderer(w io.Writer) *rowRenderer {
	return newRowRendererFor(colorprofile.Detect(w, os.Environ()))
}

// newRowRendererFor builds a rowRenderer that downsamples to p on
// write. This is the test seam: callers can force a color profile
// (e.g. colorprofile.NoTTY for plain text) instead of detecting one.
func newRowRendererFor(p colorprofile.Profile) *rowRenderer {
	return &rowRenderer{
		profile:           p,
		idStyle:           lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		blockedGlyphStyle: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		closedGlyphStyle:  lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		p0Style:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9")),
		p1Style:           lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		p2Style:           lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		p4Style:           lipgloss.NewStyle().Faint(true),
		epicChipStyle:     lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		bugChipStyle:      lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		ownerStyle:        lipgloss.NewStyle().Faint(true),
		ruleStyle:         lipgloss.NewStyle().Faint(true),
		legendStyle:       lipgloss.NewStyle().Faint(true),
	}
}

// downsample wraps w so every styled line degrades to r.profile on
// write (colors drop to the nearest representable value; NoTTY strips
// ANSI entirely).
func (r *rowRenderer) downsample(w io.Writer) io.Writer {
	return &colorprofile.Writer{Forward: w, Profile: r.profile}
}

// renderRows writes one formatted line per row to w:
//
//	<glyph> <short_id padded>  <prio>  <chips><title> (<owner>)
//
// The id column is left-aligned and padded to the widest id across
// rows (minimum 4 cells), measured with lipgloss.Width so wide
// glyphs and any embedded ANSI don't throw off alignment. Printing
// nothing for an empty slice is intentional: callers skip calling
// renderRows/footers on zero rows, but the renderer tolerates it too.
func (r *rowRenderer) renderRows(w io.Writer, rows []issueRow) error {
	if len(rows) == 0 {
		return nil
	}
	w = r.downsample(w)
	idWidth := 4
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = textsafe.Line(row.ID)
		if width := lipgloss.Width(ids[i]); width > idWidth {
			idWidth = width
		}
	}
	for i, row := range rows {
		line := r.renderRow(row, ids[i], idWidth)
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func (r *rowRenderer) renderRow(row issueRow, id string, idWidth int) string {
	glyph := r.glyph(row)
	idCell := r.idStyle.Render(id + strings.Repeat(" ", idWidth-lipgloss.Width(id)))
	prio := r.priorityField(row.Priority)
	chips := r.chipsField(row.Labels)
	title := textsafe.Line(row.Title)
	owner := r.ownerStyle.Render("(" + textsafe.Line(row.Owner) + ")")
	return glyph + " " + idCell + "  " + prio + "  " + chips + title + " " + owner
}

// glyph picks the status glyph. Closed wins outright; otherwise
// Blocked wins over the open status per the Architecture contract.
// The open glyph carries no style (default terminal fg, unstyled).
func (r *rowRenderer) glyph(row issueRow) string {
	switch {
	case row.Status == "closed":
		return r.closedGlyphStyle.Render("✓")
	case row.Blocked:
		return r.blockedGlyphStyle.Render("●")
	default:
		return "○"
	}
}

// priorityField renders the fixed 4-cell priority field. P3 and any
// unrecognized value render as unstyled default-fg text (matching
// the Visual Spec's "P3: default fg, no color"). The fixed 4-cell
// width ("• P<n>") relies on the daemon clamping priority to 0-4;
// a multi-digit priority would break column alignment.
func (r *rowRenderer) priorityField(p *int64) string {
	if p == nil {
		return "    "
	}
	text := fmt.Sprintf("• P%d", *p)
	switch *p {
	case 0:
		return r.p0Style.Render(text)
	case 1:
		return r.p1Style.Render(text)
	case 2:
		return r.p2Style.Render(text)
	case 4:
		return r.p4Style.Render(text)
	default:
		return text
	}
}

// chipsField renders the well-known label chips, epic then bug, each
// as "[label] " (trailing space). Only these two labels render;
// everything else is not a recognized chip and is dropped.
func (r *rowRenderer) chipsField(labels []string) string {
	has := func(want string) bool {
		for _, l := range labels {
			if l == want {
				return true
			}
		}
		return false
	}
	var b strings.Builder
	if has("epic") {
		b.WriteString(r.epicChipStyle.Render("[epic]"))
		b.WriteString(" ")
	}
	if has("bug") {
		b.WriteString(r.bugChipStyle.Render("[bug]"))
		b.WriteString(" ")
	}
	return b.String()
}

// renderListFooter writes the blank-line/rule/summary/blank-line/
// legend footer for `kata list`. Counts are derived from rows
// (blocked wins over open for the bucket, matching renderRow's
// glyph precedence) rather than trusting a caller-supplied total, so
// the summary always reflects what was actually printed above it.
// When truncated is true (the caller hit --limit and there may be
// more matching issues than were returned), the summary uses
// "Showing:" instead of "Total:" so it doesn't read as a full count.
func (r *rowRenderer) renderListFooter(w io.Writer, rows []issueRow, truncated bool) error {
	if len(rows) == 0 {
		return nil
	}
	var open, blocked, closed int
	for _, row := range rows {
		switch {
		case row.Status == "closed":
			closed++
		case row.Blocked:
			blocked++
		default:
			open++
		}
	}
	var clauses []string
	if open > 0 {
		clauses = append(clauses, fmt.Sprintf("%d open", open))
	}
	if blocked > 0 {
		clauses = append(clauses, fmt.Sprintf("%d blocked", blocked))
	}
	if closed > 0 {
		clauses = append(clauses, fmt.Sprintf("%d closed", closed))
	}
	total := len(rows)
	label := "Total"
	if truncated {
		label = "Showing"
	}
	summary := fmt.Sprintf("%s: %d %s (%s)", label, total, issueWord(total), strings.Join(clauses, ", "))
	return r.renderFooter(w, summary, "Status: ○ open  ● blocked  ✓ closed")
}

// renderReadyFooter writes the footer for `kata ready`. The legend
// omits "● blocked" since ready results are, by definition, unblocked.
// When truncated is true (the caller hit --limit and there may be
// more ready issues than were returned), the summary uses "Showing:"
// instead of "Ready:" so it doesn't read as a full count.
func (r *rowRenderer) renderReadyFooter(w io.Writer, n int, truncated bool) error {
	if n == 0 {
		return nil
	}
	var summary string
	if truncated {
		summary = fmt.Sprintf("Showing: %d ready %s with no active blockers", n, issueWord(n))
	} else {
		summary = fmt.Sprintf("Ready: %d %s with no active blockers", n, issueWord(n))
	}
	return r.renderFooter(w, summary, "Status: ○ open")
}

func (r *rowRenderer) renderFooter(w io.Writer, summary, legend string) error {
	w = r.downsample(w)
	rule := r.ruleStyle.Render(strings.Repeat("─", footerRuleWidth))
	legendLine := r.legendStyle.Render(legend)
	_, err := fmt.Fprintf(w, "\n%s\n%s\n\n%s\n", rule, summary, legendLine)
	return err
}

// issueWord returns the singular/plural noun for a count.
func issueWord(n int) string {
	if n == 1 {
		return "issue"
	}
	return "issues"
}
