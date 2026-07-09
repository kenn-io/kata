package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"
	"github.com/mattn/go-runewidth"
)

// ptrString is a test-only helper for taking the address of a string
// literal inline. The production model exposes pointer fields (Owner,
// DeletedAt) and Go forbids &"literal" so the fixtures need this.
func ptrString(s string) *string { return &s }

// ptrTime is the time.Time companion to ptrString.
func ptrTime(t time.Time) *time.Time { return &t }

// ptrInt64 is the *int64 companion used by Priority fixtures.
func ptrInt64(n int64) *int64 { return &n }

// listFixture is the on-screen seed for the list tests. Three rows cover
// the open, closed, and soft-deleted statusChip branches without booting
// a real daemon. The deleted row keeps statusChip's DeletedAt branch
// under test even after the Task 4 fixture moved into this file.
func listFixture() []Issue {
	deleted := time.Now().Add(-2 * time.Hour)
	return []Issue{
		{
			UID: "01TEST-aaa1", ShortID: "aaa1", Title: "fix login bug on Safari",
			Status: "open", Owner: ptrString("claude-4.7"),
			UpdatedAt: time.Now().Add(-3 * time.Hour),
		},
		{
			UID: "01TEST-bbb2", ShortID: "bbb2", Title: "rebuild search index",
			Status: "closed", Owner: ptrString("wesm"),
			UpdatedAt: time.Now().Add(-1 * time.Hour),
		},
		{
			UID: "01TEST-ccc3", ShortID: "ccc3", Title: "purge stale tokens",
			Status: "open", DeletedAt: ptrTime(deleted),
			UpdatedAt: deleted,
		},
	}
}

// setupListTeatest boots a teatest model at 120x30, seeds the standard
// listFixture, and registers a ctrl+c fast-quit cleanup. q opens the
// quit-confirm modal in M3.5b; ctrl+c bypasses the confirm so tests
// terminate without an extra `y` keystroke + race on the modal.
func setupListTeatest(t *testing.T) *teatest.TestModel {
	t.Helper()
	m := initialModel(Options{})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(120, 30))
	tm.Send(tea.WindowSizeMsg{Width: 120, Height: 30})
	tm.Send(initialFetchMsg{dispatchKey: cacheKey{limit: queueFetchLimit}, issues: listFixture()})
	t.Cleanup(func() {
		tm.Send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		tm.WaitFinished(t)
	})
	return tm
}

// TestList_Render_Fixture confirms the seed reaches the screen so the
// rendering layer can be reviewed independent of the network layer. The
// [deleted] assertion guards statusChip's soft-delete branch.
func TestList_Render_Fixture(t *testing.T) {
	tm := setupListTeatest(t)
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := string(b)
		return strings.Contains(s, "fix login bug on Safari") &&
			strings.Contains(s, "[deleted]")
	}, teatest.WithDuration(2*time.Second))
}

// TestList_Render_FitsAt80Cols locks the wide-mode column budget at the
// 80-column supported minimum: the rendered table rows must not exceed
// 80 visible cells. Adding the prio column at 5 cells previously pushed
// the sum to 81 (cursor 2 + context 2 + tree 4 + num 6 + prio 5 +
// status 10 + children 8 + owner 14 + updated 10 + title 20 = 81). With
// prio at 4 the sum stays at 80 even with the title floor.
func TestList_Render_FitsAt80Cols(t *testing.T) {
	lm := newListModel()
	lm.loading = false
	lm.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "fits at the floor", Status: "open", Priority: ptrInt64(1)},
	}
	out := lm.View(80, 30, viewChrome{})
	for i, line := range strings.Split(out, "\n") {
		w := runewidth.StringWidth(stripANSI(line))
		if w > 80 {
			t.Fatalf("line %d exceeds 80 cells: width=%d, line=%q", i, w, line)
		}
	}
}

// TestList_Render_PriorityColumn drives the priority cell through both
// the set ("P1") and unset ("—") branches at the column level so a
// regression in priorityCell or buildRows doesn't have to wait for a
// snapshot file to flag it.
func TestList_Render_PriorityColumn(t *testing.T) {
	lm := newListModel()
	lm.loading = false
	lm.issues = []Issue{
		{UID: "01TEST-aaa1", ShortID: "aaa1", Title: "with priority", Status: "open", Priority: ptrInt64(1)},
		{UID: "01TEST-bbb2", ShortID: "bbb2", Title: "without priority", Status: "open"},
	}
	out := lm.View(140, 30, viewChrome{})
	if !strings.Contains(out, "P1") {
		t.Fatalf("rendered list missing P1 priority cell:\n%s", out)
	}
	// The em dash is the unset placeholder; rendered with subtle style
	// so we strip ANSI before checking presence.
	plain := stripANSI(out)
	if !strings.Contains(plain, "—") {
		t.Fatalf("rendered list missing — for unset priority:\n%s", plain)
	}
}

// TestList_Cursor_DownAndUp drives j/j/k against the three-row fixture
// and asserts the marker glyph lands on the row containing #bbb2 (the
// second short_id in listFixture). The third row gives the down-clamp
// room to move; with two rows j/j/k would land on index 0 because the
// cursor never reaches 2. lipgloss/table pads between columns, so we
// scan output line-by-line for one that contains both the marker and
// the row's short_id.
func TestList_Cursor_DownAndUp(t *testing.T) {
	tm := setupListTeatest(t)
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return strings.Contains(string(b), "purge stale tokens")
	}, teatest.WithDuration(2*time.Second))
	tm.Send(tea.KeyPressMsg{Code: 'j', Text: "j"})
	tm.Send(tea.KeyPressMsg{Code: 'j', Text: "j"})
	tm.Send(tea.KeyPressMsg{Code: 'k', Text: "k"})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "▶") && strings.Contains(line, "#bbb2") {
				return true
			}
		}
		return false
	}, teatest.WithDuration(2*time.Second))
}
