package tui

import (
	"strings"
	"testing"
	"time"
)

// TestDetailRedesign_StackedHasProjectBarOnFirstLine asserts the
// project/title bar is the first non-empty rendered line in stacked
// detail. Regression for the screenshot bug where the global chrome
// dropped off and the issue content rendered against column 0 with
// nothing above it.
func TestDetailRedesign_StackedHasProjectBarOnFirstLine(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	got := stripANSI(dm.View(160, 32, viewChrome{
		scope:   scope{projectID: 7, projectName: "kata"},
		version: "dev",
	}))
	first := firstNonEmptyLine(got)
	if !strings.Contains(first, "Project: kata") {
		t.Fatalf("expected first line to be project bar, got %q\n%s", first, got)
	}
	if !strings.Contains(first, "kata カタ") {
		t.Fatalf("expected brand on first line, got %q\n%s", first, got)
	}
}

// TestDetailRedesign_ContentHasLeftGutter ensures issue content does
// not start at column 0. The redesign requires a 2-cell gutter so the
// page reads as a designed surface, not a raw debug dump. Excludes
// the project/title bar line (which has its own padding), the info
// line, and the footer help table.
func TestDetailRedesign_ContentHasLeftGutter(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	dm.issue.Owner = ptrString("alice")
	got := stripANSI(dm.View(160, 32, viewChrome{
		scope:   scope{projectID: 7, projectName: "kata"},
		version: "dev",
	}))
	for _, want := range []string{
		"#42",
		"fix login bug on Safari",
		"owner: alice",
		"Body",
		"Reproduces in Safari",
	} {
		row := lineContaining(got, want)
		if row == "" {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
		if !strings.HasPrefix(row, "  ") {
			t.Fatalf("row %q lacks 2-cell left gutter:\n%q", want, row)
		}
	}
}

// TestDetailRedesign_NoReplacementGlyphs is a regression for the
// screenshot's `��` artifacts. The renderer must never emit the
// Unicode replacement character regardless of width, scope, or empty
// metadata fields.
func TestDetailRedesign_NoReplacementGlyphs(t *testing.T) {
	defer snapshotInit(t)()
	dm := snapDetailFixture()
	cases := []struct {
		name     string
		w, h     int
		applyDM  func(detailModel) detailModel
		applyChr func(viewChrome) viewChrome
	}{
		{
			name: "wide single project no labels no children",
			w:    160, h: 32,
			applyDM: func(d detailModel) detailModel {
				d.issue.Labels = nil
				d.children = nil
				return d
			},
			applyChr: func(c viewChrome) viewChrome {
				c.scope = scope{projectID: 7, projectName: "kata"}
				c.version = "dev"
				return c
			},
		},
		{
			name: "narrow no chrome",
			w:    72, h: 24,
			applyDM:  func(d detailModel) detailModel { return d },
			applyChr: func(c viewChrome) viewChrome { return c },
		},
		{
			name: "default 80x40",
			w:    80, h: 40,
			applyDM:  func(d detailModel) detailModel { return d },
			applyChr: func(c viewChrome) viewChrome { return c },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testDM := tc.applyDM(dm)
			chrome := tc.applyChr(viewChrome{})
			got := stripANSI(testDM.View(tc.w, tc.h, chrome))
			if strings.ContainsRune(got, '�') {
				t.Fatalf("output contains replacement glyph U+FFFD:\n%s", got)
			}
		})
	}
}

// TestDetailRedesign_OmitsEmptyLabelsAndChildren keeps empty metadata
// out of the page. Per the redesign spec, `labels: none` and
// `children: none` should never render — they consume attention with
// no signal. Owner and parent stay (those absences are informative).
func TestDetailRedesign_OmitsEmptyLabelsAndChildren(t *testing.T) {
	defer snapshotInit(t)()
	dm := simpleDetailModel()
	got := stripANSI(dm.View(160, 24, viewChrome{
		scope: scope{projectID: 7, projectName: "kata"},
	}))
	assertStringsLack(t, got, "labels: none", "children: none")
}

// TestDetailRedesign_DefaultsToFirstNonEmptyActivityTab covers the
// requirement that on first open, the activity section selects the
// first tab with entries instead of stopping on an empty Comments
// tab. Mirrors the screenshot's "Comments (0) active while Events (1)
// has data" failure.
func TestDetailRedesign_DefaultsToFirstNonEmptyActivityTab(t *testing.T) {
	defer snapshotInit(t)()
	dm := simpleDetailModel()
	when := time.Date(2026, 5, 2, 19, 16, 0, 0, time.UTC)
	dm = seedActivity(dm, nil, []EventLogEntry{
		{ID: 1, Type: "issue.created", Actor: "anonymous", CreatedAt: when},
	}, nil)
	if dm.activeTab != tabEvents {
		t.Fatalf("expected activeTab=%v after fetch with empty comments + 1 event, got %v",
			tabEvents, dm.activeTab)
	}
	got := stripANSI(dm.View(120, 32, viewChrome{}))
	if !strings.Contains(got, "[ Events (1) ]") {
		t.Fatalf("expected Events tab to be marked active:\n%s", got)
	}
}

// TestDetailRedesign_DefaultStaysWhenAllActivityEmpty: when nothing
// has data, activeTab keeps its default (Comments) so the placeholder
// tab strip still reads naturally. Guards against an over-eager auto-
// switch that would jump tabs on initial load before fetches complete.
func TestDetailRedesign_DefaultStaysWhenAllActivityEmpty(t *testing.T) {
	defer snapshotInit(t)()
	dm := seedActivity(simpleDetailModel(), nil, nil, nil)
	if dm.activeTab != tabComments {
		t.Fatalf("expected activeTab to stay tabComments when all empty, got %v",
			dm.activeTab)
	}
}

// TestDetailRedesign_ExplicitTabPickStaysSticky: once the user picks
// a tab explicitly, fetch results must not override it. Without this
// guard, a late-arriving events fetch would yank focus away from the
// user's chosen tab.
func TestDetailRedesign_ExplicitTabPickStaysSticky(t *testing.T) {
	defer snapshotInit(t)()
	dm := simpleDetailModel()
	// User explicitly cycles to tabLinks before any fetch arrives.
	dm.activeTab = tabLinks
	dm.tabExplicit = true
	when := time.Date(2026, 5, 2, 19, 16, 0, 0, time.UTC)
	dm = dm.applyFetched(commentsFetchedMsg{gen: dm.gen, comments: nil})
	dm = dm.applyFetched(eventsFetchedMsg{
		gen: dm.gen,
		events: []EventLogEntry{
			{ID: 1, Type: "issue.created", Actor: "x", CreatedAt: when},
		},
	})
	if dm.activeTab != tabLinks {
		t.Fatalf("explicit tab pick should be sticky, got %v", dm.activeTab)
	}
}

// TestDetailRedesign_FooterHintsAreComprehensive ensures the
// persistent detail footer surfaces every detail-mode action so the
// user is never stranded looking for a binding. The footer is
// expected to wrap across multiple rows on narrow terminals via
// reflowHelpRows; this test only checks the row content, not the
// rendered width.
func TestDetailRedesign_FooterHintsAreComprehensive(t *testing.T) {
	dm := detailModel{
		issue:       &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "issue", Status: "open"},
		detailFocus: focusActivity,
		activeTab:   tabComments,
	}
	rows := dm.detailHelpRows()
	keys := map[string]bool{}
	for _, row := range rows {
		for _, item := range row {
			keys[item.key+" "+item.desc] = true
		}
	}
	for _, want := range []string{
		"↑↓ scroll", "j/k row", "↹ section", "↵ open", "pgup/pgdn page",
		"e edit", "c comment", "+ label", "- unlabel",
		"a owner", "A unassign",
		"x close", "r reopen",
		"p parent", "b block", "l related", "N child",
		"L layout",
		"esc back", "? help", "q quit",
	} {
		if !keys[want] {
			t.Fatalf("expected detail footer to carry %q; got rows=%+v", want, rows)
		}
	}
}

// TestDetailRedesign_SectionHeadersHaveNoBackgroundSlab: the Body /
// Activity section labels must not paint a background slab. The
// previous design wrapped them in detailSectionHeaderStyle with an
// adaptive background; the redesign drops the background so labels
// read as plain bold text against the page surface.
func TestDetailRedesign_SectionHeadersHaveNoBackgroundSlab(t *testing.T) {
	defer applyDefaultColorMode()
	applyColorMode(colorDark, true)
	if styleHasBackground(detailSectionHeaderStyle) {
		t.Fatal("detailSectionHeaderStyle should not paint a background slab")
	}
	if styleHasBackground(detailMetaStyle) {
		t.Fatal("detailMetaStyle should not paint a background band")
	}
}

// simpleDetailModel returns a minimal detailModel for tab/state tests
// where issue contents are incidental — only the embedded *Issue
// pointer is required to drive view and tab logic.
func simpleDetailModel() detailModel {
	iss := Issue{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "issue", Status: "open"}
	return detailModel{issue: &iss}
}

// seedActivity applies the three activity-fetch messages in order so
// tests can drive the post-fetch state in one line. Pass nil for any
// tab that should remain empty.
func seedActivity(dm detailModel, comments []CommentEntry, events []EventLogEntry, links []LinkEntry) detailModel {
	dm = dm.applyFetched(commentsFetchedMsg{gen: dm.gen, comments: comments})
	dm = dm.applyFetched(eventsFetchedMsg{gen: dm.gen, events: events})
	dm = dm.applyFetched(linksFetchedMsg{gen: dm.gen, links: links})
	return dm
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			return line
		}
	}
	return ""
}

func lineContaining(s, needle string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
