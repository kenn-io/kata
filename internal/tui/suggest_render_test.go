package tui

import (
	"errors"
	"strings"
	"testing"
)

// defaultTestPrompt builds the inputLabelPrompt panel state shared by
// every suggest-render test in this file: project 7, issue 1.
func defaultTestPrompt() inputState {
	return newPanelPrompt(inputLabelPrompt, formTarget{
		projectID: 7, issueShortID: "aaa1",
	})
}

// TestSuggestMenu_RendersEntries: a populated cache produces a menu
// with one row per suggestion (top + bottom border + N entry rows).
func TestSuggestMenu_RendersEntries(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	suggestions := []LabelCount{
		{Label: "bug", Count: 5},
		{Label: "design", Count: 3},
	}
	got := renderSuggestMenu(s, suggestions, labelCacheEntry{})
	assertContains(t, got, "bug", "menu missing 'bug'")
	assertContains(t, got, "design", "menu missing 'design'")
}

// TestSuggestMenu_LoadingPlaceholder: a fetching=true entry with no
// labels renders the loading placeholder instead of an empty list.
func TestSuggestMenu_LoadingPlaceholder(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	got := renderSuggestMenu(s, nil, labelCacheEntry{
		pid: 7, gen: 1, fetching: true,
	})
	assertContains(t, got, "loading", "menu missing 'loading' placeholder")
}

// TestSuggestMenu_ErrorPlaceholder: an entry with a non-nil err
// surfaces the error message in the menu body.
func TestSuggestMenu_ErrorPlaceholder(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	got := renderSuggestMenu(s, nil, labelCacheEntry{
		pid: 7, gen: 1, err: errors.New("daemon 500"),
	})
	assertContains(t, got, "daemon 500", "menu missing error message")
	assertContains(t, got, "error", "menu missing error label")
}

// TestSuggestMenu_EmptyPlaceholder: a fetched entry with zero labels
// surfaces the "no labels" hint so the user knows the project has
// no labels yet (rather than a confusingly-empty menu).
func TestSuggestMenu_EmptyPlaceholder(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	got := renderSuggestMenu(s, nil, labelCacheEntry{
		pid: 7, gen: 1, fetching: false,
	})
	assertContains(t, got, "no labels", "menu missing empty placeholder")
}

// TestSuggestMenu_Scrolls_HighlightStaysVisible: with N > maxRows
// suggestions and the highlight at the end, the visible window
// scrolls so the highlighted row is rendered (would be off-screen
// without the windowing).
func TestSuggestMenu_Scrolls_HighlightStaysVisible(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	s.suggestHighlight = 9 // out past the menu's row budget
	suggestions := make([]LabelCount, 12)
	for i := range suggestions {
		suggestions[i] = LabelCount{
			Label: "lbl-" + ptrFormat(int64(i+1)),
			Count: int64(12 - i),
		}
	}
	got := renderSuggestMenu(s, suggestions, labelCacheEntry{})
	assertContains(t, got, "lbl-10",
		"scroll window did not include highlighted row (lbl-10)")
	// And the first entries (which would render without scroll)
	// should NOT be present once the window has scrolled past them.
	assertNotContains(t, got, "lbl-1\n", "scroll window did not scroll past lbl-1")
	assertNotContains(t, got, "lbl-1 ", "scroll window did not scroll past lbl-1")
}

// TestFilterSuggestions_PrefixCaseInsensitive: prefix filter matches
// case-insensitively and ignores leading whitespace in the prefix.
func TestFilterSuggestions_PrefixCaseInsensitive(t *testing.T) {
	all := []LabelCount{
		{Label: "Bug", Count: 5},
		{Label: "design", Count: 3},
		{Label: "BugFix", Count: 2},
	}
	got := filterSuggestions(all, "BUG")
	if len(got) != 2 {
		t.Fatalf("want 2 matches for BUG, got %d: %+v", len(got), got)
	}
}

// TestFilterSuggestions_SortsByCountThenLabel: count desc primary,
// label asc secondary. Ties on count fall to alphabetical.
func TestFilterSuggestions_SortsByCountThenLabel(t *testing.T) {
	all := []LabelCount{
		{Label: "z", Count: 1},
		{Label: "a", Count: 5},
		{Label: "m", Count: 5},
		{Label: "b", Count: 1},
	}
	got := filterSuggestions(all, "")
	want := []string{"a", "m", "b", "z"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Label != w {
			t.Fatalf("got[%d].Label = %q, want %q (full: %+v)",
				i, got[i].Label, w, got)
		}
	}
}

// TestOverlayAtCorner_PlacesAtAnchor: a 1x1 panel placed at (row, col)
// shows up in the bg at the right cell.
func TestOverlayAtCorner_PlacesAtAnchor(t *testing.T) {
	bg := strings.Join([]string{
		"......",
		"......",
		"......",
	}, "\n")
	got := overlayAtCorner(bg, "X", 6, 3, 1, 2)
	want := strings.Join([]string{
		"......",
		"..X...",
		"......",
	}, "\n")
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// TestSuggestMenu_InfoLineAndFooterStayAtBottom_WhenMenuOpen pins the
// load-bearing layout invariant: when the suggestion menu is open, the
// info line MUST sit on row height-2 (carrying the active panel-prompt
// prefix) and the footer help row MUST sit on row height-1. The menu
// may occupy rows ABOVE the info line, but it must NEVER overlay or
// push past either of those bottom rows.
//
// This is the test that should have caught Plan-8 commit 3's C1 — the
// detail layout shrunk the body+tab budget by menuH AND the overlay
// computed anchorRow against the original height, so info+footer slid
// up by menuH while the menu was anchored relative to the natural
// bottom — collision at the info row, entries past the footer.
func TestSuggestMenu_InfoLineAndFooterStayAtBottom_WhenMenuOpen(t *testing.T) {
	defer snapshotInit(t)()
	m := snapLabelPromptModel()
	m.projectLabels.byProject[7] = labelCacheEntry{
		pid: 7, gen: 1,
		labels: []LabelCount{
			{Label: "alpha", Count: 5},
			{Label: "beta", Count: 4},
			{Label: "gamma", Count: 3},
		},
	}
	out := m.viewContent()
	lines := strings.Split(out, "\n")
	if len(lines) < m.height {
		t.Fatalf("View returned %d lines, want >= %d", len(lines), m.height)
	}
	infoRow := lines[m.height-2]
	footerRow := lines[m.height-1]
	// Info line must carry the active prompt prefix.
	if !strings.Contains(infoRow, "add label to #42") {
		t.Fatalf("info-line row (height-2 = %d) lost the prompt prefix.\n"+
			"got: %q\nfull view:\n%s", m.height-2, infoRow, out)
	}
	// Footer must carry the prompt's commit/cancel help.
	if !strings.Contains(footerRow, "enter") || !strings.Contains(footerRow, "commit") {
		t.Fatalf("footer row (height-1 = %d) lost the help row.\n"+
			"got: %q\nfull view:\n%s", m.height-1, footerRow, out)
	}
	// Menu border-top character `┌` MUST appear ABOVE the info line.
	// If the menu top lands on info-row OR below, the layout is wrong.
	topBorder := -1
	for i, ln := range lines {
		if strings.Contains(ln, "┌") {
			topBorder = i
			break
		}
	}
	if topBorder < 0 {
		t.Fatalf("menu top border `┌` not found in view:\n%s", out)
	}
	if topBorder >= m.height-2 {
		t.Fatalf("menu top border landed on row %d (info = %d, footer = %d)"+
			" — menu must float ABOVE info/footer.\nfull view:\n%s",
			topBorder, m.height-2, m.height-1, out)
	}
	// And the menu's bottom border `└` must be at most one row above
	// the info line (height-3) — the spec says "menu's bottom row =
	// info-line row - 1".
	botBorder := -1
	for i, ln := range lines {
		if strings.Contains(ln, "└") {
			botBorder = i
		}
	}
	if botBorder < 0 {
		t.Fatalf("menu bottom border `└` not found in view:\n%s", out)
	}
	if botBorder != m.height-3 {
		t.Fatalf("menu bottom border at row %d, want %d "+
			"(one row above info line at %d).\nfull view:\n%s",
			botBorder, m.height-3, m.height-2, out)
	}
}

// TestSuggestMenuHeight_CountsBordersAndBody: the height includes
// the top/bottom borders + body rows (max of visible entries vs.
// placeholder rows).
func TestSuggestMenuHeight_CountsBordersAndBody(t *testing.T) {
	defer snapshotInit(t)()
	s := defaultTestPrompt()
	// Empty cache: 1 placeholder row + 2 borders = 3.
	if got := suggestMenuHeight(s, nil, labelCacheEntry{}); got != 3 {
		t.Fatalf("empty-cache height = %d, want 3", got)
	}
	// 4 entries: 4 body rows + 2 borders = 6.
	suggestions := make([]LabelCount, 4)
	for i := range suggestions {
		suggestions[i] = LabelCount{Label: "x" + ptrFormat(int64(i)), Count: 1}
	}
	if got := suggestMenuHeight(s, suggestions, labelCacheEntry{}); got != 6 {
		t.Fatalf("4-entry height = %d, want 6", got)
	}
}
