package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// labelPromptFixture builds a Model with an open label prompt and a
// pre-populated suggestion cache. Used by the interaction tests so
// they exercise key dispatch against a populated menu.
func labelPromptFixture() Model {
	pid := int64(7)
	m := Model{
		view:          viewDetail,
		scope:         scope{projectID: pid},
		projectLabels: newLabelCache(),
		toastNow:      func() time.Time { return time.Time{} },
	}
	m.detail.scopePID = pid
	m.detail.gen = 1
	m.detail.issue = &Issue{ProjectID: pid, UID: "01TEST-42aa", ShortID: "42aa", Status: "open"}
	m.projectLabels.byProject[pid] = labelCacheEntry{
		pid: pid, gen: 1,
		labels: []LabelCount{
			{Label: "alpha", Count: 5},
			{Label: "beta", Count: 3},
			{Label: "gamma", Count: 1},
		},
	}
	m.input = newPanelPrompt(inputLabelPrompt, formTarget{
		projectID: pid, issueShortID: "42aa", detailGen: 1,
	})
	return m
}

// assertHighlight fails the test unless the suggestion menu's
// highlight index matches want.
func assertHighlight(t *testing.T, m Model, want int) {
	t.Helper()
	if got := m.input.suggestHighlight; got != want {
		t.Fatalf("suggestHighlight = %d, want %d", got, want)
	}
}

// TestLabelPrompt_ArrowKeys_MoveHighlight_WithWrap: pressing ↓ four
// times wraps from index 0 → 1 → 2 → 0 (3 entries). Then ↑ wraps
// 0 → 2.
func TestLabelPrompt_ArrowKeys_MoveHighlight_WithWrap(t *testing.T) {
	m := labelPromptFixture()
	m = sendKey(m, tea.KeyDown)
	assertHighlight(t, m, 1)
	m = sendKey(m, tea.KeyDown)
	assertHighlight(t, m, 2)
	// Down wraps to 0.
	m = sendKey(m, tea.KeyDown)
	assertHighlight(t, m, 0)
	// Up wraps from 0 → 2.
	m = sendKey(m, tea.KeyUp)
	assertHighlight(t, m, 2)
}

// TestLabelPrompt_TabCompletesHighlightedSuggestion: with the
// highlight on entry index 1 (beta — second after sort), pressing
// Tab fills the buffer with "beta".
func TestLabelPrompt_TabCompletesHighlightedSuggestion(t *testing.T) {
	m := labelPromptFixture()
	// Move highlight to beta (sorted by count desc: alpha=5, beta=3,
	// gamma=1, so beta is index 1).
	m = sendKey(m, tea.KeyDown)
	m = sendKey(m, tea.KeyTab)
	if got := m.input.activeField().value(); got != "beta" {
		t.Fatalf("buffer = %q after tab on beta, want %q", got, "beta")
	}
}

// TestLabelPrompt_EmptyBuffer_ShowsTopProjectLabels: with an empty
// buffer, the suggestion source is unfiltered and the count-desc
// sort applies (alpha=5 first).
func TestLabelPrompt_EmptyBuffer_ShowsTopProjectLabels(t *testing.T) {
	m := labelPromptFixture()
	got := filterSuggestions(m.suggestionsForPrompt(m.input), "")
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Label != "alpha" {
		t.Fatalf("first label = %q, want %q (count-desc sort)",
			got[0].Label, "alpha")
	}
}

// TestLabelPrompt_PrefixFilterCaseInsensitive: an "AL" prefix
// matches "alpha" but not "beta" (case-insensitive).
func TestLabelPrompt_PrefixFilterCaseInsensitive(t *testing.T) {
	m := labelPromptFixture()
	got := filterSuggestions(m.suggestionsForPrompt(m.input), "AL")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 for prefix AL", len(got))
	}
	if got[0].Label != "alpha" {
		t.Fatalf("got %q, want %q", got[0].Label, "alpha")
	}
}

// TestRemoveLabelPrompt_SourceIsAttachedLabelsNotProjectCache: the
// `-` prompt sources from dm.issue.Labels, NOT from the project
// cache. Even with a populated cache, the suggestion list reflects
// only what's currently attached.
func TestRemoveLabelPrompt_SourceIsAttachedLabelsNotProjectCache(t *testing.T) {
	m := labelPromptFixture()
	m.detail.issue.Labels = []string{"attached1", "attached2"}
	m.input = newPanelPrompt(inputRemoveLabelPrompt, formTarget{
		projectID: 7, issueShortID: "42aa",
	})
	got := m.suggestionsForPrompt(m.input)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Label != "attached1" || got[1].Label != "attached2" {
		t.Fatalf("got %+v, want attached1 + attached2", got)
	}
	// And the cache labels (alpha/beta/gamma) must not bleed in.
	for _, lc := range got {
		if lc.Label == "alpha" || lc.Label == "beta" {
			t.Fatalf("removeLabelPrompt source leaked project cache: %+v", got)
		}
	}
}

// TestLabelPrompt_EnterCommitsCurrentBuffer: pressing Enter with a
// free-typed buffer dispatches the label-add mutation (commit
// closes the input and routes through commitInput → dispatchLabel).
func TestLabelPrompt_EnterCommitsCurrentBuffer(t *testing.T) {
	m := labelPromptFixture()
	m.input.activeField().input.SetValue("freshlabel")
	m.input.fields[0] = *m.input.activeField()
	nm := sendKey(m, tea.KeyEnter)
	assertInputKind(t, nm, inputNone)
}

// TestLabelPrompt_EscClosesPromptAndMenu: esc cancels the input,
// closing both the prompt and the menu.
func TestLabelPrompt_EscClosesPromptAndMenu(t *testing.T) {
	m := labelPromptFixture()
	nm := sendKey(m, tea.KeyEsc)
	assertInputKind(t, nm, inputNone)
}

// TestSuggestMenu_BodyKeepsFullHeight_AndChromeStaysAtBottom: when
// the menu is open, the detail body+tab area MUST keep its full
// vertical budget (no menuH subtraction). The menu OVERLAYS tab
// content; the info line stays at row height-2 and the footer at
// row height-1. Tightened from the prior "withMenu != noMenu" check
// (which only proved input.kind swapped the footer text) to assert
// the load-bearing layout invariant directly — paired with C1's
// TestSuggestMenu_InfoLineAndFooterStayAtBottom_WhenMenuOpen, which
// pins the same shape at the full Model.View() level.
func TestSuggestMenu_BodyKeepsFullHeight_AndChromeStaysAtBottom(t *testing.T) {
	defer snapshotInit(t)()
	m := labelPromptFixture()
	dm := m.detail
	dm.comments = []CommentEntry{
		{ID: 1, Author: "a", Body: "x"},
		{ID: 2, Author: "b", Body: "y"},
		{ID: 3, Author: "c", Body: "z"},
	}
	dm.activeTab = tabComments
	const h = 30
	withMenu := dm.View(120, h, m.chrome())
	noMenu := dm.View(120, h, viewChrome{})
	withLines := strings.Split(withMenu, "\n")
	noLines := strings.Split(noMenu, "\n")
	if len(withLines) != h || len(noLines) != h {
		t.Fatalf("dm.View row counts: withMenu=%d, noMenu=%d, want %d each",
			len(withLines), len(noLines), h)
	}
	// The activity rule must land at the same row in both renders —
	// the body+tab budget MUST not change when the menu is active.
	withRule := indexOf(withLines, "Activity")
	noRule := indexOf(noLines, "Activity")
	if withRule != noRule || withRule < 0 {
		t.Fatalf("activity rule moved with menu (withMenu row=%d, "+
			"noMenu row=%d) — the body must not shrink when menu opens",
			withRule, noRule)
	}
}

// indexOf returns the row index of the first line containing prefix,
// or -1 when not found.
func indexOf(lines []string, prefix string) int {
	for i, ln := range lines {
		if strings.Contains(ln, prefix) {
			return i
		}
	}
	return -1
}
