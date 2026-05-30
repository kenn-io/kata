package tui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// narrowHintMarker is the substring that uniquely identifies the
// renderTooNarrow hint screen. Centralized so the test bodies stay
// short and a future copy edit only touches one place.
const narrowHintMarker = "needs more space"

// narrowTestSetup pins the package to KATA_COLOR_MODE=none so View
// output is plain UTF-8, then returns a fresh Model with no loading
// flag set. The cleanup func reverts the color rebuild.
func narrowTestSetup(t *testing.T) (Model, func()) {
	t.Helper()
	t.Setenv("KATA_COLOR_MODE", "none")
	t.Setenv("NO_COLOR", "")
	applyDefaultColorMode(io.Discard)
	m := initialModel(Options{})
	m.list.loading = false
	cleanup := func() { applyDefaultColorMode(io.Discard) }
	return m, cleanup
}

// assertHintVisible fails t when the rendered View's hint-marker
// presence doesn't match want. Folds the verbose Contains+Fatalf
// pattern that the narrow-terminal tests repeat for both polarities.
func assertHintVisible(t *testing.T, m Model, want bool) {
	t.Helper()
	view := m.View()
	got := strings.Contains(view, narrowHintMarker)
	if got == want {
		return
	}
	if want {
		t.Fatalf("view missing hint marker %q; got:\n%s", narrowHintMarker, view)
	}
	t.Fatalf("view unexpectedly contains hint marker %q; got:\n%s", narrowHintMarker, view)
}

// TestNarrowTerminal_NarrowWidthShowsHint verifies that a sub-80-column
// width trips the M5 short-circuit and renders the centered hint
// regardless of how tall the terminal is.
func TestNarrowTerminal_NarrowWidthShowsHint(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()

	m = resizeModel(m, 60, 24)
	assertHintVisible(t, m, true)
}

// TestNarrowTerminal_ShortHeightRendersNormally verifies that a short
// terminal still renders the active view when width is sufficient. The
// list view has compact rendering; blocking on height makes tmux panes
// unnecessarily unusable.
func TestNarrowTerminal_ShortHeightRendersNormally(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()
	m.list.issues = []Issue{{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "short pane", Status: "open"}}
	m.scope = scope{projectID: 7, projectName: "kata"}

	m = resizeModel(m, 120, 12)
	assertHintVisible(t, m, false)
	if got := m.View(); !strings.Contains(got, "short pane") {
		t.Fatalf("short-height view missing list content; got:\n%s", got)
	}
}

// TestNarrowTerminal_BothNarrowShowsHint covers the width gate when
// height is also short. Width below threshold is still unreadable.
func TestNarrowTerminal_BothNarrowShowsHint(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()

	m = resizeModel(m, 60, 12)
	assertHintVisible(t, m, true)
}

// TestNarrowTerminal_NormalSizeRendersNormally verifies the negative
// case: a comfortably-sized terminal should NOT short-circuit. We
// assert the hint marker is absent rather than checking for a
// specific list/detail substring so the test stays robust against
// future chrome wording changes.
func TestNarrowTerminal_NormalSizeRendersNormally(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()

	m = resizeModel(m, 120, 30)
	assertHintVisible(t, m, false)
}

// TestNarrowTerminal_QStillRoutesToQuitConfirm proves that the View
// short-circuit doesn't break key routing — q at narrow size still
// opens the quit-confirm modal via routeGlobalKey. This is the
// primary safety invariant: the user must be able to quit even when
// the terminal is too small to render normally.
func TestNarrowTerminal_QStillRoutesToQuitConfirm(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()

	m = resizeModel(m, 60, 12)
	nm, cmd := updateModel(m, runeKey('q'))
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatal("q at narrow size produced tea.Quit; should have opened the confirm modal")
			}
		}
	}
	if nm.modal != modalQuitConfirm {
		t.Fatalf("modal = %v at narrow size, want modalQuitConfirm", nm.modal)
	}
}

// TestNarrowTerminal_CtrlCStillQuits proves ctrl+c remains the
// power-user immediate-quit even when the hint is up. Mirrors
// TestQuit_CtrlCFastQuits but at sub-threshold dimensions.
func TestNarrowTerminal_CtrlCStillQuits(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()

	m = resizeModel(m, 60, 12)
	_, cmd := updateModel(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("ctrl+c at narrow size produced no cmd; expected tea.Quit")
	}
	if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
		t.Fatalf("ctrl+c cmd at narrow size = %T, want tea.QuitMsg", cmd())
	}
}

// TestNarrowTerminal_ZeroWidthBeforeFirstResize_DoesNotShowHint pins
// the `m.width > 0` gate: before the first WindowSizeMsg arrives,
// initialModel leaves width=0 (and height=0). Without the gate, the
// hint would flash on every cold start. We assert View renders the
// normal body (no hint marker) so the boot path stays clean.
func TestNarrowTerminal_ZeroWidthBeforeFirstResize_DoesNotShowHint(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()
	// No WindowSizeMsg; m.width and m.height remain 0.
	assertHintVisible(t, m, false)
}

// TestNarrowTerminal_QuitConfirmModalOverlaysHint covers roborev #250:
// when the user opens the quit-confirm modal then resizes below the
// chrome threshold, the modal must remain visible on top of the hint
// (not silently swallowed by the short-circuit). Without the overlay
// the user would be stuck — pressing q would only re-open the
// invisible modal and ctrl+c would be the only escape.
//
// Both centered on the same axis, the modal's text covers the hint
// text, but the hint's normal-border outline (┌/└) shows around the
// modal's rounded-border (╭/╰), so the user knows both layers are
// present. We assert on the rounded corner ╭ as the modal-only
// marker (the hint uses sharp corners), and on the hint's sharp
// corner ┌ which still pokes out around the smaller modal.
func TestNarrowTerminal_QuitConfirmModalOverlaysHint(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()
	// Resize to full width and open quit-confirm.
	m = resizeModel(m, 120, 30)
	m = sendRune(m, 'q')
	if m.modal != modalQuitConfirm {
		t.Fatalf("modal = %v after q at full width, want modalQuitConfirm", m.modal)
	}
	// Now resize below threshold while the modal is still open.
	m = resizeModel(m, 60, 24)
	view := m.View()
	if !strings.Contains(view, "[Y]") {
		t.Fatalf("quit-confirm modal hidden by narrow short-circuit; got:\n%s", view)
	}
	if !strings.Contains(view, "Quit kata?") {
		t.Fatalf("modal title missing after narrow resize; got:\n%s", view)
	}
	// The hint's sharp top corner ┌ pokes out next to the modal's
	// rounded ╭, so both layers are visible to the user.
	if !strings.Contains(view, "┌") {
		t.Fatalf("narrow hint border missing under modal; got:\n%s", view)
	}
	if !strings.Contains(view, "╭") {
		t.Fatalf("modal rounded border missing; got:\n%s", view)
	}
}

func TestNarrowTerminal_DaemonViewShowsHintAndToast(t *testing.T) {
	m, cleanup := narrowTestSetup(t)
	defer cleanup()
	m = resizeModel(m, 60, 24)
	m.view = viewDaemons
	m.daemonTargets = []daemonTarget{{Name: "shared", URL: "https://daemon.example"}}
	m.toast = &toast{text: "daemon failed", level: toastError}

	view := m.View()

	if !strings.Contains(view, narrowHintMarker) {
		t.Fatalf("daemon view at narrow width must show stable too-narrow hint; got:\n%s", view)
	}
	if strings.Contains(view, "kata / daemons") {
		t.Fatalf("daemon view rendered full picker below minimum width; got:\n%s", view)
	}
	if !strings.Contains(view, "daemon failed") {
		t.Fatalf("daemon switch failure toast hidden at narrow width; got:\n%s", view)
	}
}
