package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSITest(s string) string {
	return ansiSequence.ReplaceAllString(s, "")
}

// gutter is the literal two-space separator the Visual Spec places
// between the id/prio/chips fields. noPrio is the 4-cell empty
// priority field ("<prio>" is a fixed 4-cell field).
const gutter = "  "
const noPrio = "    "

// row builds the expected plain-text line for a color-off row so test
// expectations are computed from the spec's field layout rather than
// hand-counted whitespace:
//
//	<glyph> <id padded><gutter><prio><gutter><chips><title> (<owner>)
func row(glyph, idPadded, prio, chips, title, owner string) string {
	return glyph + " " + idPadded + gutter + prio + gutter + chips + title + " (" + owner + ")\n"
}

func TestRenderRows_ColorOff(t *testing.T) {
	r := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))

	t.Run("open row with P1 and epic chip", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:       "abc1",
			Title:    "fix the thing",
			Owner:    "alice",
			Priority: ptrInt64(1),
			Status:   "open",
			Labels:   []string{"epic"},
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", "• P1", "[epic] ", "fix the thing", "alice"), buf.String())
	})

	t.Run("blocked row", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:      "abc1",
			Title:   "waiting on dep",
			Owner:   "bob",
			Status:  "open",
			Blocked: true,
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("●", "abc1", noPrio, "", "waiting on dep", "bob"), buf.String())
	})

	t.Run("closed row", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:     "abc1",
			Title:  "done deal",
			Owner:  "carol",
			Status: "closed",
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("✓", "abc1", noPrio, "", "done deal", "carol"), buf.String())
	})

	t.Run("closed but also blocked: closed glyph wins", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:      "abc1",
			Title:   "done deal",
			Owner:   "carol",
			Status:  "closed",
			Blocked: true,
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("✓", "abc1", noPrio, "", "done deal", "carol"), buf.String())
	})

	t.Run("nil priority row", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:     "abc1",
			Title:  "no priority",
			Owner:  "dave",
			Status: "open",
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", noPrio, "", "no priority", "dave"), buf.String())
	})

	t.Run("id width alignment across mixed-width ids", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{
			{ID: "a1", Title: "short id", Owner: "eve", Status: "open"},
			{ID: "abcdefgh", Title: "long id", Owner: "eve", Status: "open"},
		}
		require.NoError(t, r.renderRows(&buf, rows))
		want := row("○", "a1"+strings.Repeat(" ", 6), noPrio, "", "short id", "eve") +
			row("○", "abcdefgh", noPrio, "", "long id", "eve")
		assert.Equal(t, want, buf.String())
	})

	t.Run("bug chip", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:     "abc1",
			Title:  "crash on load",
			Owner:  "eve",
			Status: "open",
			Labels: []string{"bug"},
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", noPrio, "[bug] ", "crash on load", "eve"), buf.String())
	})

	t.Run("epic and bug chips together, ordered, non-chip labels ignored", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:     "abc1",
			Title:  "big scary thing",
			Owner:  "eve",
			Status: "open",
			Labels: []string{"bug", "epic", "other"},
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", noPrio, "[epic] [bug] ", "big scary thing", "eve"), buf.String())
	})

	t.Run("p4 priority", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:       "abc1",
			Title:    "low priority",
			Owner:    "eve",
			Priority: ptrInt64(4),
			Status:   "open",
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", "• P4", "", "low priority", "eve"), buf.String())
	})

	t.Run("zero rows prints nothing", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderRows(&buf, nil))
		assert.Empty(t, buf.String())
	})

	t.Run("title newline is sanitized single-line", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{
			ID:     "abc1",
			Title:  "line one\nline two",
			Owner:  "eve",
			Status: "open",
		}}
		require.NoError(t, r.renderRows(&buf, rows))
		assert.Equal(t, row("○", "abc1", noPrio, "", `line one\nline two`, "eve"), buf.String())
	})
}

func TestRenderListFooter_ColorOff(t *testing.T) {
	r := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))

	const rule = "──────────────────────────────────────────────────"
	const legend = "Status: ○ open  ● blocked  ✓ closed"

	t.Run("mixed counts", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{
			{ID: "a1", Status: "open"},
			{ID: "a2", Status: "open", Blocked: true},
			{ID: "a3", Status: "closed"},
		}
		require.NoError(t, r.renderListFooter(&buf, rows, false))
		want := "\n" + rule + "\n" + "Total: 3 issues (1 open, 1 blocked, 1 closed)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("singular, all open", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{ID: "a1", Status: "open"}}
		require.NoError(t, r.renderListFooter(&buf, rows, false))
		want := "\n" + rule + "\n" + "Total: 1 issue (1 open)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("no closed, no blocked clauses when zero", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{
			{ID: "a1", Status: "open"},
			{ID: "a2", Status: "closed"},
		}
		require.NoError(t, r.renderListFooter(&buf, rows, false))
		want := "\n" + rule + "\n" + "Total: 2 issues (1 open, 1 closed)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("open+blocked counts as blocked only", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{
			{ID: "a1", Status: "open", Blocked: true},
		}
		require.NoError(t, r.renderListFooter(&buf, rows, false))
		want := "\n" + rule + "\n" + "Total: 1 issue (1 blocked)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("zero rows prints nothing", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderListFooter(&buf, nil, false))
		assert.Empty(t, buf.String())
	})

	t.Run("truncated uses Showing wording, not Total", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{
			{ID: "a1", Status: "open"},
			{ID: "a2", Status: "open", Blocked: true},
			{ID: "a3", Status: "closed"},
		}
		require.NoError(t, r.renderListFooter(&buf, rows, true))
		want := "\n" + rule + "\n" + "Showing: 3 issues (1 open, 1 blocked, 1 closed)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("truncated singular uses Showing wording", func(t *testing.T) {
		var buf bytes.Buffer
		rows := []issueRow{{ID: "a1", Status: "open"}}
		require.NoError(t, r.renderListFooter(&buf, rows, true))
		want := "\n" + rule + "\n" + "Showing: 1 issue (1 open)\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})
}

func TestRenderReadyFooter_ColorOff(t *testing.T) {
	r := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))

	const rule = "──────────────────────────────────────────────────"
	const legend = "Status: ○ open"

	t.Run("plural", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderReadyFooter(&buf, 3, false))
		want := "\n" + rule + "\n" + "Ready: 3 issues with no active blockers\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("singular", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderReadyFooter(&buf, 1, false))
		want := "\n" + rule + "\n" + "Ready: 1 issue with no active blockers\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("zero prints nothing", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderReadyFooter(&buf, 0, false))
		assert.Empty(t, buf.String())
	})

	t.Run("truncated plural uses Showing wording", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderReadyFooter(&buf, 3, true))
		want := "\n" + rule + "\n" + "Showing: 3 ready issues with no active blockers\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})

	t.Run("truncated singular uses Showing wording", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, r.renderReadyFooter(&buf, 1, true))
		want := "\n" + rule + "\n" + "Showing: 1 ready issue with no active blockers\n" + "\n" + legend + "\n"
		assert.Equal(t, want, buf.String())
	})
}

// TestRenderRows_ColorOn pins the exact ANSI-containing output for one
// representative row so regressions in the escape sequences are loud.
// It also asserts that stripping the ANSI from the color-ON row yields
// byte-identical output to the color-OFF rendering (layout invariance).
func TestRenderRows_ColorOn(t *testing.T) {
	lr := lipgloss.NewRenderer(&bytes.Buffer{})
	lr.SetColorProfile(termenv.ANSI256)
	r := newRowRendererFor(lr)

	rows := []issueRow{{
		ID:       "abc1",
		Title:    "fix the thing",
		Owner:    "alice",
		Priority: ptrInt64(1),
		Status:   "open",
		Labels:   []string{"epic"},
	}}
	var buf bytes.Buffer
	require.NoError(t, r.renderRows(&buf, rows))

	const want = "○ \x1b[36mabc1\x1b[0m  \x1b[31m• P1\x1b[0m  \x1b[35m[epic]\x1b[0m " +
		"fix the thing \x1b[2m(alice)\x1b[0m\n"
	assert.Equal(t, want, buf.String())

	off := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))
	var offBuf bytes.Buffer
	require.NoError(t, off.renderRows(&offBuf, rows))
	assert.Equal(t, offBuf.String(), stripANSITest(buf.String()))
}

// TestRenderListFooter_ColorOn pins the rule's and legend's exact ANSI
// output (both faint).
func TestRenderListFooter_ColorOn(t *testing.T) {
	lr := lipgloss.NewRenderer(&bytes.Buffer{})
	lr.SetColorProfile(termenv.ANSI256)
	r := newRowRendererFor(lr)

	rows := []issueRow{{ID: "a1", Status: "open"}}
	var buf bytes.Buffer
	require.NoError(t, r.renderListFooter(&buf, rows, false))

	const rule = "──────────────────────────────────────────────────"
	want := "\n" +
		"\x1b[2m" + rule + "\x1b[0m\n" +
		"Total: 1 issue (1 open)\n" +
		"\n" +
		"\x1b[2mStatus: ○ open  ● blocked  ✓ closed\x1b[0m\n"
	assert.Equal(t, want, buf.String())

	off := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))
	var offBuf bytes.Buffer
	require.NoError(t, off.renderListFooter(&offBuf, rows, false))
	assert.Equal(t, offBuf.String(), stripANSITest(buf.String()))
}

// TestRenderRows_WideGlyphID covers a row whose id contains wide glyphs
// and confirms id-column alignment uses cell width, not len(), and does
// not panic. "カタ1" is 5 cells wide (2 + 2 + 1).
func TestRenderRows_WideGlyphID(t *testing.T) {
	r := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))

	rows := []issueRow{
		{ID: "カタ1", Title: "wide id row", Owner: "eve", Status: "open"},
		{ID: "a1", Title: "narrow id row", Owner: "eve", Status: "open"},
	}
	var buf bytes.Buffer
	require.NoError(t, r.renderRows(&buf, rows))

	want := row("○", "カタ1", noPrio, "", "wide id row", "eve") +
		row("○", "a1"+strings.Repeat(" ", 3), noPrio, "", "narrow id row", "eve")
	assert.Equal(t, want, buf.String())
}

// TestRenderRows_WideGlyphTitle covers a wide-glyph title on a row
// whose id is not the widest, confirming it renders without panicking
// and doesn't perturb id-column alignment (the id column precedes the
// title, so only id padding affects alignment).
func TestRenderRows_WideGlyphTitle(t *testing.T) {
	r := newRowRendererFor(lipgloss.NewRenderer(&bytes.Buffer{}, termenv.WithProfile(termenv.Ascii)))

	rows := []issueRow{
		{ID: "abcdefgh", Title: "long id row", Owner: "eve", Status: "open"},
		{ID: "a1", Title: "カタ祭り", Owner: "eve", Status: "open"},
	}
	var buf bytes.Buffer
	require.NotPanics(t, func() {
		require.NoError(t, r.renderRows(&buf, rows))
	})

	want := row("○", "abcdefgh", noPrio, "", "long id row", "eve") +
		row("○", "a1"+strings.Repeat(" ", 6), noPrio, "", "カタ祭り", "eve")
	assert.Equal(t, want, buf.String())
}

func TestNewRowRenderer(t *testing.T) {
	var buf bytes.Buffer
	r := newRowRenderer(&buf)
	require.NotNil(t, r)
}
