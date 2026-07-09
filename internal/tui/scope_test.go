package tui

import "testing"

// TestEmptyState_RendersHint: viewEmpty renders the onboarding hint
// containing both the "no kata projects" line and the kata init hint.
func TestEmptyState_RendersHint(t *testing.T) {
	m := setupEmptyView()
	assertContainsAll(t, m.viewContent(),
		"no kata projects registered yet",
		"kata init",
		"press q to quit",
	)
}

// TestEmptyState_QuitsOnQ: q from viewEmpty opens the M3.5b
// quit-confirm modal. Y from there commits to quit. ctrl+c remains
// the immediate-quit escape hatch so the user is never trapped.
func TestEmptyState_QuitsOnQ(t *testing.T) {
	m, _ := updateModel(setupEmptyView(), keyRune('q'))
	if m.modal != modalQuitConfirm {
		t.Fatalf("q from viewEmpty did not open quit-confirm: %v", m.modal)
	}
	_, cmd := updateModel(m, keyRune('y'))
	assertCmdQuit(t, cmd)
}

// TestEmptyState_OtherKeysIgnored: j, ?, R in viewEmpty are no-ops so an
// unbound user can't accidentally fall into a partially-functional help
// or list view from a state with no projects.
func TestEmptyState_OtherKeysIgnored(t *testing.T) {
	m := setupEmptyView()
	for _, k := range []rune{'j', '?', 'R', 'k', 's'} {
		out, cmd := updateModel(m, keyRune(k))
		if out.view != viewEmpty {
			t.Fatalf("key %q changed view from empty to %v", k, out.view)
		}
		if cmd != nil {
			t.Fatalf("key %q in viewEmpty returned non-nil cmd", k)
		}
	}
}

// TestRenderEmpty_ZeroDims: a zero-sized terminal renders the message
// without panicking inside lipgloss.Place. Defensive: the model emits
// width/height from WindowSizeMsg, which can lag on first frame.
func TestRenderEmpty_ZeroDims(t *testing.T) {
	assertContainsAll(t, renderEmpty(0, 0), "no kata projects registered yet")
}
