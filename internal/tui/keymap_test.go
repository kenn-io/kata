package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestKeymapEmacsAliases(t *testing.T) {
	km := newKeymap()
	tests := []struct {
		name string
		key  key
		msg  tea.KeyPressMsg
	}{
		{"forward", km.PageDown, tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl, Text: "v"}},
		{"back alt", km.PageUp, tea.KeyPressMsg{Code: 'v', Mod: tea.ModAlt, Text: "v"}},
		{"back meta", km.PageUp, tea.KeyPressMsg{Code: 'v', Mod: tea.ModMeta, Text: "v"}},
		{"start alt direct", km.Home, tea.KeyPressMsg{Code: '<', Mod: tea.ModCtrl | tea.ModAlt, Text: "<"}},
		{"start alt shifted", km.Home, tea.KeyPressMsg{Code: ',', ShiftedCode: '<', Mod: tea.ModCtrl | tea.ModAlt | tea.ModShift, Text: "<"}},
		{"start meta direct", km.Home, tea.KeyPressMsg{Code: '<', Mod: tea.ModCtrl | tea.ModMeta, Text: "<"}},
		{"start meta shifted", km.Home, tea.KeyPressMsg{Code: ',', ShiftedCode: '<', Mod: tea.ModCtrl | tea.ModMeta | tea.ModShift, Text: "<"}},
		{"end alt direct", km.End, tea.KeyPressMsg{Code: '>', Mod: tea.ModCtrl | tea.ModAlt, Text: ">"}},
		{"end alt shifted", km.End, tea.KeyPressMsg{Code: '.', ShiftedCode: '>', Mod: tea.ModCtrl | tea.ModAlt | tea.ModShift, Text: ">"}},
		{"end meta direct", km.End, tea.KeyPressMsg{Code: '>', Mod: tea.ModCtrl | tea.ModMeta, Text: ">"}},
		{"end meta shifted", km.End, tea.KeyPressMsg{Code: '.', ShiftedCode: '>', Mod: tea.ModCtrl | tea.ModMeta | tea.ModShift, Text: ">"}},
		{"existing start", km.Home, tea.KeyPressMsg{Code: 'g', Text: "g"}},
		{"existing end", km.End, tea.KeyPressMsg{Code: 'G', Text: "G"}},
		{"shift-only", km.End, tea.KeyPressMsg{Code: 'g', ShiftedCode: 'G', Mod: tea.ModShift, Text: "G"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.key.matches(tt.msg) {
				t.Fatalf("%s (%s) did not match", tt.msg.String(), tt.msg.Keystroke())
			}
		})
	}
	for _, msg := range []tea.KeyPressMsg{
		{Code: 'v', Mod: tea.ModCtrl, Text: "v"},
		{Code: 'v', Mod: tea.ModAlt, Text: "v"},
		{Code: 'v', Mod: tea.ModMeta, Text: "v"},
	} {
		if km.ToggleIssueView.matches(msg) {
			t.Errorf("modified v (%s) toggled list layout", msg.Keystroke())
		}
	}
	if !km.ToggleIssueView.matches(tea.KeyPressMsg{Code: 'v', Text: "v"}) {
		t.Fatal("plain v stopped toggling list layout")
	}
}

func TestKeymapEmacsVerticalMovement(t *testing.T) {
	km := newKeymap()
	for _, tt := range []struct {
		name string
		key  key
		msg  tea.KeyPressMsg
	}{
		{"down", km.ScrollDown, tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl, Text: "n"}},
		{"up", km.ScrollUp, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl, Text: "p"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.key.matches(tt.msg) {
				t.Fatalf("%s did not match %s", tt.name, tt.msg.Keystroke())
			}
		})
	}
	if km.NewIssue.matches(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl, Text: "n"}) {
		t.Fatal("C-n matched new issue")
	}
	if km.SetParent.matches(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl, Text: "p"}) {
		t.Fatal("C-p matched set parent")
	}
}

func TestKeymapEmacsSectionCycling(t *testing.T) {
	km := newKeymap()
	next := tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl, Text: "j"}
	prev := tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl, Text: "k"}
	if !km.NextTab.matches(next) || !km.PrevTab.matches(prev) {
		t.Fatalf("section chords did not match: %s / %s", next.Keystroke(), prev.Keystroke())
	}
	if km.Down.matches(next) || km.Up.matches(prev) {
		t.Fatal("section chords matched plain row movement")
	}
	if !km.Down.matches(tea.KeyPressMsg{Code: 'j', Text: "j"}) ||
		!km.Up.matches(tea.KeyPressMsg{Code: 'k', Text: "k"}) {
		t.Fatal("plain j/k stopped moving rows")
	}
}
