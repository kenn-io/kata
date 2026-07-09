package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func mouseLeftClick(x, y int) tea.MouseMsg {
	return tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft}
}

func mouseWheelDown() tea.MouseMsg {
	return mouseWheelDownAt(0)
}

func mouseWheelDownAt(x int) tea.MouseMsg {
	return tea.MouseWheelMsg{X: x, Button: tea.MouseWheelDown}
}

func mouseWheelUp() tea.MouseMsg {
	return tea.MouseWheelMsg{Button: tea.MouseWheelUp}
}

func TestViewMouseModeFollowsOption(t *testing.T) {
	without := initialModel(Options{}).View()
	if without.MouseMode != tea.MouseModeNone {
		t.Fatalf("mouse mode without opt-in = %v, want MouseModeNone", without.MouseMode)
	}
	with := initialModel(Options{Mouse: true}).View()
	if with.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("mouse mode with opt-in = %v, want MouseModeCellMotion", with.MouseMode)
	}
	if !with.AltScreen || !without.AltScreen {
		t.Fatal("TUI must always declare the alt screen on its View")
	}
}

func TestMouseDisabledIgnoresMouseMsg(t *testing.T) {
	m := resizeModel(newTestModel(), 120, 30)
	m.list.issues = makeTestIssues(3)
	m.list.cursor = 0

	nm, cmd := updateModel(m, mouseLeftClick(4, 6))
	if cmd != nil {
		t.Fatalf("disabled mouse returned cmd %T, want nil", cmd())
	}
	if nm.list.cursor != 0 {
		t.Fatalf("disabled mouse moved cursor to %d, want 0", nm.list.cursor)
	}
}

func TestMouseClickSelectsIssueListRow(t *testing.T) {
	m := resizeModel(newTestModel(), 120, 30)
	m.opts.Mouse = true
	m.list.issues = makeTestIssues(5)

	nm, cmd := updateModel(m, mouseLeftClick(4, 6)) // stacked list row 3: title, stats, header, rule, row0...
	if cmd != nil {
		t.Fatalf("single click returned cmd %T, want nil", cmd())
	}
	if nm.list.cursor != 2 || nm.list.selectedUID != "01TEST-r003" {
		t.Fatalf("cursor=%d selected=%q, want cursor=2 selected r003", nm.list.cursor, nm.list.selectedUID)
	}
}

func TestMouseSplitClickFirstIssueRowSelectsFirstVisibleRow(t *testing.T) {
	m := resizeModel(newTestModel(), 160, 30)
	m.opts.Mouse = true
	m.layout = layoutSplit
	m.list.issues = makeTestIssues(5)
	m.list.cursor = 0

	nm, _ := updateModel(m, mouseLeftClick(4, 4)) // title, pane border, table header, rule, first row
	if nm.list.cursor != 0 || nm.list.selectedUID != "01TEST-r001" {
		t.Fatalf("cursor=%d selected=%q, want first visible issue", nm.list.cursor, nm.list.selectedUID)
	}
}

func TestMouseFullScreenHelpIgnoresSplitPaneMouse(t *testing.T) {
	m := resizeModel(newTestModel(), 160, 30)
	m.opts.Mouse = true
	m.layout = layoutSplit
	m.view = viewHelp
	m.list.issues = makeTestIssues(5)
	m.list.cursor = 0

	nm, _ := updateModel(m, mouseWheelDownAt(4))
	if nm.list.cursor != 0 {
		t.Fatalf("help-view mouse wheel moved list cursor to %d, want no-op", nm.list.cursor)
	}
}

func TestMouseWheelScrollsIssueDetail(t *testing.T) {
	m := setupDetailScenario(t, 120, 30, "line\nline\nline\nline\nline\nline\nline\nline\nline\nline\nline\nline\nline\nline\nline\nTAIL")
	m.opts.Mouse = true
	m.detail.scroll = 0

	nm, _ := updateModel(m, mouseWheelDown())
	if nm.detail.scroll == 0 {
		t.Fatal("wheel down did not scroll detail body")
	}
	nm, _ = updateModel(nm, mouseWheelUp())
	if nm.detail.scroll != 0 {
		t.Fatalf("wheel up scroll=%d, want 0", nm.detail.scroll)
	}
}

func TestMouseClickSelectsProjectsRow(t *testing.T) {
	m := setupProjectsView(
		mockProject{ID: 1, Name: "alpha", Ident: "github.com/acme/alpha"},
		mockProject{ID: 2, Name: "beta", Ident: "github.com/acme/beta"},
	)
	m.opts.Mouse = true

	nm, cmd := updateModel(m, mouseLeftClick(2, 5)) // projects row after title/count/blank/header
	if cmd != nil {
		t.Fatalf("single click returned cmd %T, want nil", cmd())
	}
	if nm.projectsCursor != 1 {
		t.Fatalf("projectsCursor=%d, want first concrete project row", nm.projectsCursor)
	}
}
