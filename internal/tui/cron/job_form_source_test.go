package cron

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func editorKey(m *jobForm, key string) tea.Cmd {
	_, cmd := m.Update(press(key))
	return cmd
}

func chooseField(t *testing.T, m *jobForm, key string) {
	t.Helper()
	for i, f := range m.fields() {
		if f.Key == key {
			m.index = i
			return
		}
	}
	t.Fatal("missing field", key)
}

func TestEditorDirtyExit(t *testing.T) {
	clean := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	if cmd := editorKey(clean, "esc"); cmd == nil {
		t.Fatal("clean exit prompts")
	}
	m := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	editorKey(m, "enter")
	editorKey(m, "example")
	if editorKey(m, "esc") != nil || !strings.Contains(m.View(), "discard changes? y/n") {
		t.Fatal("dirty exit skipped prompt", m.View())
	}
	editorKey(m, "n")
	editorKey(m, " job")
	editorKey(m, "ctrl+s")
	if m.Value("name") != "example job" {
		t.Fatal("discard n lost active input")
	}
	editorKey(m, "esc")
	if cmd := editorKey(m, "y"); cmd == nil {
		t.Fatal("discard did not exit")
	}
	restored := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	original := restored.Value("prompt")
	restored.Set("prompt", "changed")
	if !restored.dirtyDraft() {
		t.Fatal("editable change did not dirty draft")
	}
	restored.Set("prompt", original)
	if editorKey(restored, "q") == nil {
		t.Fatal("restored draft remained dirty")
	}
	m.phase = phaseSaving
	if editorKey(m, "esc") != nil {
		t.Fatal("saving escaped")
	}
}

func TestEditorFieldErrorKeepsBuffer(t *testing.T) {
	m := newJobForm(t.Context(), nil, nil, nil, editorContext{})
	m.Set("trigger", "interval")
	chooseField(t, m, "schedule")
	editorKey(m, "enter")
	editorKey(m, "1.5s")
	editorKey(m, "ctrl+s")
	if m.Value("schedule") == "1.5s" || m.active != "schedule" || m.buffer != "1.5s" || m.caret != 4 || !strings.Contains(m.fieldErrors["schedule"], "whole seconds only") {
		t.Fatalf("failed input lost: active=%s buffer=%q caret=%d errors=%v", m.active, m.buffer, m.caret, m.fieldErrors)
	}
}

func TestScheduleLabelMatchesTrigger(t *testing.T) {
	for trigger, want := range map[string]string{"cron": "Cron", "interval": "Interval", "once": "Run at"} {
		t.Run(trigger, func(t *testing.T) {
			m := newJobForm(t.Context(), nil, nil, nil, editorContext{})
			m.Set("trigger", trigger)
			for _, f := range m.fields() {
				if f.Key == "schedule" {
					if f.Label != want {
						t.Fatalf("got %q, want %q", f.Label, want)
					}
					return
				}
			}
			t.Fatal("schedule field missing")
		})
	}
}

func press(s string) tea.KeyPressMsg {
	named := map[string]rune{"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "space": tea.KeySpace, "backspace": tea.KeyBackspace, "delete": tea.KeyDelete, "up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight, "home": tea.KeyHome, "end": tea.KeyEnd, "pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown}
	var mod tea.KeyMod
	name := s
	for {
		prefix, rest, ok := strings.Cut(name, "+")
		if !ok || rest == "" || (prefix != "ctrl" && prefix != "shift" && prefix != "alt") {
			break
		}
		mod |= map[string]tea.KeyMod{"ctrl": tea.ModCtrl, "shift": tea.ModShift, "alt": tea.ModAlt}[prefix]
		name = rest
	}
	if code, ok := named[name]; ok {
		k := tea.KeyPressMsg{Code: code, Mod: mod}
		if code == tea.KeySpace && mod == 0 {
			k.Text = " "
		}
		return k
	}
	r := []rune(name)
	if len(r) == 1 && mod != 0 {
		return tea.KeyPressMsg{Code: r[0], Mod: mod}
	}
	if len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: name}
	}
	return tea.KeyPressMsg{Code: tea.KeyExtended, Text: s}
}
