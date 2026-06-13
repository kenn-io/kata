package tui

import (
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
)

func TestQueueHelpRows_ConditionalItems(t *testing.T) {
	withChildren := Model{list: listModel{issues: hierarchyIssues()}}
	assertHelpItemsPresent(t, withChildren.queueHelpRows(),
		helpItem{key: "space", desc: "expand"},
		helpItem{key: "E", desc: "all"},
		helpItem{key: "N", desc: "child"},
		helpItem{key: "o", desc: "order"})

	leaf := Model{list: listModel{issues: []Issue{
		{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "leaf", Status: "open"},
	}}}
	assertHelpItemAbsent(t, flattenHelpRows(leaf.queueHelpRows()),
		helpItem{key: "space", desc: "expand"})
	assertHelpItemAbsent(t, flattenHelpRows(leaf.queueHelpRows()),
		helpItem{key: "E", desc: "all"})
	assertHelpItemPresent(t, flattenHelpRows(leaf.queueHelpRows()),
		helpItem{key: "N", desc: "child"})

	flat := withChildren
	flat.list.viewMode = issueListViewFlat
	assertHelpItemAbsent(t, flattenHelpRows(flat.queueHelpRows()),
		helpItem{key: "E", desc: "all"})

	empty := Model{}
	assertHelpItemAbsent(t, flattenHelpRows(empty.queueHelpRows()),
		helpItem{key: "N", desc: "child"})
}

// TestDetailHelpRows_Contexts: the persistent detail footer is
// comprehensive — every detail-mode binding handled by the Update
// loop appears so the user is not stranded looking for an action.
// The activity-focus row carries navigation primitives plus the
// full mutation surface (edit/comment/label/owner/parent/blocker/
// link/close/reopen/quit). Children focus swaps the navigation
// header (↑↓ child / ↵ open child) but keeps the action surface.
func TestDetailHelpRows_Contexts(t *testing.T) {
	activity := Model{detail: detailModel{
		issue:       &Issue{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "issue", Status: "open"},
		detailFocus: focusActivity,
		activeTab:   tabComments,
	}}
	assertHelpItemsPresent(t, activity.detailHelpRows(),
		helpItem{key: "↑↓", desc: "scroll"},
		helpItem{key: "j/k", desc: "row"},
		helpItem{key: "↹", desc: "section"},
		helpItem{key: "↵", desc: "open"},
		helpItem{key: "pgup/pgdn", desc: "page"},
		helpItem{key: "e", desc: "edit"},
		helpItem{key: "c", desc: "comment"},
		helpItem{key: "+", desc: "label"},
		helpItem{key: "a", desc: "owner"},
		helpItem{key: "x", desc: "close"},
		helpItem{key: "r", desc: "reopen"},
		helpItem{key: "p", desc: "parent"},
		helpItem{key: "b", desc: "block"},
		helpItem{key: "l", desc: "related"},
		helpItem{key: "L", desc: "layout"},
		helpItem{key: "esc", desc: "back"},
		helpItem{key: "?", desc: "help"},
		helpItem{key: "q", desc: "quit"})

	children := Model{detail: hierarchyDetailModel(focusChildren)}
	assertHelpItemsPresent(t, children.detailHelpRows(),
		helpItem{key: "↑↓", desc: "scroll"},
		helpItem{key: "j/k", desc: "child"},
		helpItem{key: "↵", desc: "open child"},
		helpItem{key: "N", desc: "child"},
		helpItem{key: "e", desc: "edit"},
		helpItem{key: "x", desc: "close"},
		helpItem{key: "?", desc: "help"},
		helpItem{key: "q", desc: "quit"})
}

func TestHelpRows_InputAndModalContexts(t *testing.T) {
	tests := []struct {
		name string
		m    Model
		want []helpItem
	}{
		{
			name: "search bar",
			m:    Model{input: inputState{kind: inputSearchBar}},
			want: []helpItem{
				{key: "enter", desc: "commit"},
				{key: "esc", desc: "cancel"},
				{key: "ctrl+u", desc: "clear"},
			},
		},
		{
			name: "filter form",
			m:    Model{input: inputState{kind: inputFilterForm}},
			want: []helpItem{
				{key: "ctrl+s", desc: "apply"},
				{key: "esc", desc: "cancel"},
				{key: "ctrl+r", desc: "reset"},
			},
		},
		{
			name: "quit modal",
			m:    Model{modal: modalQuitConfirm},
			want: []helpItem{
				{key: "y", desc: "confirm"},
				{key: "n/esc", desc: "cancel"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertHelpItemsPresent(t, tt.m.helpRows(), tt.want...)
		})
	}
}

// TestPersistentHelpRowsPreferArrowNotation guards the queue (list)
// footer's arrow-first convention. The detail footer additionally
// surfaces j/k as the section-cursor binding, since the unified
// document refactor split arrows (viewport scroll) from j/k (row
// cursor) — both must be discoverable from the persistent help row.
func TestPersistentHelpRowsPreferArrowNotation(t *testing.T) {
	m := Model{
		list:   listModel{issues: hierarchyIssues()},
		detail: hierarchyDetailModel(focusActivity),
	}
	for _, item := range flattenHelpRows(m.queueHelpRows()) {
		if strings.Contains(item.key, "j/k") {
			t.Fatalf("queue footer keys should use arrows, got %+v", item)
		}
	}
}

func TestRenderHelpTable_ReflowsToFitWidth80(t *testing.T) {
	rows := [][]helpItem{{
		{key: "↑↓", desc: "move"},
		{key: "↵", desc: "open"},
		{key: "space", desc: "expand"},
		{key: "N", desc: "child"},
		{key: "/", desc: "search"},
		{key: "f", desc: "filter"},
		{key: "s", desc: "status"},
		{key: "c", desc: "clear"},
		{key: "x", desc: "close"},
		{key: "?", desc: "help"},
		{key: "q", desc: "quit"},
	}}
	got := stripANSI(renderHelpTable(rows, 80))
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "▕")
	assertStringContains(t, got, "space expand")
	assertStringContains(t, got, "q quit")
}

func TestReflowHelpRows_ExtremeNarrowFallsBackToOneItemPerRow(t *testing.T) {
	rows := [][]helpItem{{
		{key: "↑↓", desc: "move"},
		{key: "↵", desc: "open"},
		{key: "space", desc: "expand"},
	}}
	got := reflowHelpRows(rows, 8)
	for _, row := range got {
		if len(row) > 1 {
			t.Fatalf("narrow reflow should use one item per row, got %+v", got)
		}
	}
}

func TestListViewFooterUsesAdaptiveHelpTable(t *testing.T) {
	lm := listModel{issues: hierarchyIssues()}
	got := stripANSI(lm.View(80, 14, viewChrome{}))
	assertLineCount(t, got, 14)
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "space expand")
	assertStringContains(t, got, "N child")
	assertStringContains(t, got, "▕")
}

func TestDetailViewFooterUsesAdaptiveChildrenFocusHints(t *testing.T) {
	dm := hierarchyDetailModel(focusChildren)
	got := stripANSI(dm.View(80, 18, viewChrome{}))
	assertLineCount(t, got, 18)
	assertLinesFitWidth(t, got, 80)
	assertStringContains(t, got, "open child")
	assertStringContains(t, got, "N child")
}

// hierarchyIssues returns a parent (ShortID=p001) and one child (ShortID=c002)
// under ProjectID 7, the standard fixture for tests that need a queue
// or detail view with a parent/child relationship.
func hierarchyIssues() []Issue {
	parentSID := "p001"
	return []Issue{
		{ProjectID: 7, UID: "01TEST-p001", ShortID: parentSID, Title: "parent", Status: "open"},
		{ProjectID: 7, UID: "01TEST-c002", ShortID: "c002", Parent: &LinkPeer{UID: "01TEST-" + parentSID, ShortID: parentSID}, Title: "child", Status: "open"},
	}
}

func hierarchyDetailModel(focus detailFocus) detailModel {
	return detailModel{
		issue:       &Issue{UID: "01TEST-p001", ShortID: "p001", Title: "parent", Status: "open"},
		children:    []Issue{{UID: "01TEST-c002", ShortID: "c002", Title: "child", Status: "open"}},
		detailFocus: focus,
	}
}

func flattenHelpRows(rows [][]helpItem) []helpItem {
	out := []helpItem{}
	for _, row := range rows {
		out = append(out, row...)
	}
	return out
}

func assertHelpItemPresent(t *testing.T, rows []helpItem, want helpItem) {
	t.Helper()
	for _, row := range rows {
		if row == want {
			return
		}
	}
	t.Fatalf("help rows missing %+v in %+v", want, rows)
}

func assertHelpItemsPresent(t *testing.T, rows [][]helpItem, wants ...helpItem) {
	t.Helper()
	flat := flattenHelpRows(rows)
	for _, want := range wants {
		assertHelpItemPresent(t, flat, want)
	}
}

func assertHelpItemAbsent(t *testing.T, rows []helpItem, deny helpItem) {
	t.Helper()
	for _, row := range rows {
		if row == deny {
			t.Fatalf("help rows unexpectedly contain %+v in %+v", deny, rows)
		}
	}
}

func assertStringContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output missing %q:\n%s", want, got)
	}
}

func assertLineCount(t *testing.T, got string, want int) {
	t.Helper()
	if lines := strings.Split(got, "\n"); len(lines) != want {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), want, got)
	}
}

func assertLinesFitWidth(t *testing.T, got string, width int) {
	t.Helper()
	for i, line := range strings.Split(got, "\n") {
		if w := runewidth.StringWidth(line); w > width {
			t.Fatalf("line %d width=%d exceeds %d:\n%s", i+1, w, width, got)
		}
	}
}

func assertStringsLack(t *testing.T, got string, denials ...string) {
	t.Helper()
	for _, deny := range denials {
		if strings.Contains(got, deny) {
			t.Fatalf("output unexpectedly contains %q:\n%s", deny, got)
		}
	}
}

func assertContainsAll(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

// assertMaxGap finds the first lines containing topMarker and bottomMarker
// and fails if the bottom line trails the top by more than maxGap rows.
func assertMaxGap(t *testing.T, got, topMarker, bottomMarker string, maxGap int) {
	t.Helper()
	lines := strings.Split(got, "\n")
	top := indexOf(lines, topMarker)
	bottom := indexOf(lines, bottomMarker)
	if top < 0 || bottom < 0 {
		t.Fatalf("missing markers (%q=%d, %q=%d):\n%s", topMarker, top, bottomMarker, bottom, got)
	}
	if gap := bottom - top; gap > maxGap {
		t.Fatalf("%q→%q gap=%d exceeds %d:\n%s", topMarker, bottomMarker, gap, maxGap, got)
	}
}
