package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// scenarios_test.go drives the full Model dispatch path (Update +
// View) for the most-traveled user interactions. The plain unit tests
// (e.g. dm.handleUp directly) cover sub-component state transitions
// and the snapshot tests (testdata/golden/*) pin rendered output for
// fixed states; what's missing in between is the layer where keys
// hit the top-level Model.Update, get routed through the global /
// input / view dispatchers, mutate state, and the rendered output
// reflects the change. The body-scroll bug fixed in a583806 was
// reachable only through this layer — handleUp/handleDown was
// state-correct in unit tests, and snapshots of one focus state
// looked fine, but pressing PgDn on a populated detail produced no
// observable change.
//
// We drive Model.Update directly (instead of through teatest's
// program runner) because teatest's output buffer is consumed by
// each WaitFor call, which made post-keystroke assertions race with
// boot-time output. Direct Update lets the test step through
// state synchronously and inspect the latest View() with no buffer
// drama. Each scenario stays under 50 lines and runs in single-digit
// milliseconds.

// scenarioModel returns a Model + screen size pair seeded with the
// listFixture issues in a single project, ready to render the list
// view at the given size.
func scenarioModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := initialModel(Options{})
	m.api = nil
	m.scope = scope{projectID: 7, projectName: "kata"}
	m.width, m.height = w, h
	m.layout = pickLayout(w, h)
	out, _ := m.Update(initialFetchMsg{
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      listFixture(),
	})
	return out.(Model)
}

// setupDetailScenario is the most common opening: build a list-view
// model at (w, h) and immediately transition to detail for the first
// fixture issue with the given body.
func setupDetailScenario(t *testing.T, w, h int, body string) Model {
	t.Helper()
	return scenarioOpenDetail(t, scenarioModel(t, w, h), body)
}

// assertViewState pins the layout/view/focus triple in one assertion
// so failures print all three values in one message instead of
// stopping at the first mismatch.
func assertViewState(t *testing.T, m Model, l layoutMode, v viewID, f focusPane) {
	t.Helper()
	if m.layout != l || m.view != v || m.focus != f {
		t.Fatalf("state mismatch: got layout=%v view=%v focus=%v, want %v/%v/%v",
			m.layout, m.view, m.focus, l, v, f)
	}
}

// assertViewContains fails if the rendered (ANSI-stripped) view does
// not contain substr. Includes the full view in the failure message
// so the test output shows what was actually rendered.
func assertViewContains(t *testing.T, m Model, substr string) {
	t.Helper()
	if !strings.Contains(view(m), substr) {
		t.Fatalf("expected view to contain %q; view:\n%s", substr, view(m))
	}
}

// assertViewMissing is the negative counterpart of assertViewContains.
func assertViewMissing(t *testing.T, m Model, substr string) {
	t.Helper()
	if strings.Contains(view(m), substr) {
		t.Fatalf("expected view to NOT contain %q; view:\n%s", substr, view(m))
	}
}

// scenarioOpenDetail puts the model into detail view for the first
// list issue and seeds the four per-tab fetch responses so the detail
// is fully populated. body is the issue body — pass a long body when
// scrolling is the test's subject so the renderer's clamp doesn't
// short-circuit the scroll. comments/events/links default to a small
// non-empty set so the activity tab strip is realistic (the bug
// being regressed was reachable only when the activity tab had rows).
func scenarioOpenDetail(t *testing.T, m Model, body string) Model {
	t.Helper()
	iss := listFixture()[0]
	iss.Body = body
	out, _ := m.Update(openDetailMsg{issue: iss})
	m = out.(Model)
	gen := m.detail.gen
	out, _ = m.Update(detailFetchedMsg{gen: gen, issue: &iss})
	m = out.(Model)
	out, _ = m.Update(commentsFetchedMsg{
		gen: gen,
		comments: []CommentEntry{
			{ID: 1, Author: "alice", Body: "first comment"},
		},
	})
	m = out.(Model)
	out, _ = m.Update(eventsFetchedMsg{
		gen: gen,
		events: []EventLogEntry{
			{ID: 9, Type: "issue.created", Actor: "alice", CreatedAt: time.Now()},
		},
	})
	m = out.(Model)
	out, _ = m.Update(linksFetchedMsg{gen: gen, links: nil})
	return out.(Model)
}

// pressKey dispatches a tea.KeyPressMsg through Model.Update and returns
// the new model. Drains a single follow-up Cmd via the same loop
// pattern Update would use; for the scenarios in this file the
// follow-ups are nil (key handlers in scope return nil cmds) so the
// drain is defensive.
func pressKey(t *testing.T, m Model, msg tea.KeyPressMsg) Model {
	t.Helper()
	out, _ := m.Update(msg)
	return out.(Model)
}

func pressRune(t *testing.T, m Model, r rune) Model {
	t.Helper()
	return pressKey(t, m, tea.KeyPressMsg{Code: r, Text: string(r)})
}

// pressN repeats a key message n times. Convenience for scrolling
// tests where one PgDn is not enough to reach the bottom.
func pressN(t *testing.T, m Model, msg tea.KeyPressMsg, n int) Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m = pressKey(t, m, msg)
	}
	return m
}

// view is sugar for stripANSI(m.viewContent()). Most assertions are easier
// against plain UTF-8 because color modes can differ between dev and
// CI but the textual layout is the same.
func view(m Model) string { return stripANSI(m.viewContent()) }

// TestScenario_ReadAnIssue_BodyScrollsOnPageDown is the regression
// for the body-scroll bug fixed in a583806. handleUp/handleDown
// never reached the dm.scroll branch when the activity tab had rows
// (the realistic case — every issue has at least an issue.created
// event), so PageDown is the dedicated body-scroll key. Snapshot
// tests didn't catch it because the bug was about an interaction,
// not a static frame.
func TestScenario_ReadAnIssue_BodyScrollsOnPageDown(t *testing.T) {
	body := strings.Repeat("scrollable line\n", 60) + "TAIL_MARKER"
	m := setupDetailScenario(t, 120, 30, body)
	assertViewMissing(t, m, "TAIL_MARKER")
	m = pressN(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown}, 16)
	assertViewContains(t, m, "TAIL_MARKER")
}

// TestScenario_ReadAnIssue_PageUpClampsAtTop ensures the body-scroll
// helpers never produce a negative offset: PgUp at the top scrolls
// past zero only if the clamp is broken, in which case the renderer
// would slice an empty window and lose the body's first line.
func TestScenario_ReadAnIssue_PageUpClampsAtTop(t *testing.T) {
	body := "FIRST_LINE_MARKER\n" + strings.Repeat("filler line\n", 30)
	m := setupDetailScenario(t, 120, 30, body)
	m = pressN(t, m, tea.KeyPressMsg{Code: tea.KeyPgUp}, 5)
	assertViewContains(t, m, "FIRST_LINE_MARKER")
	if m.detail.scroll != 0 {
		t.Fatalf("dm.scroll = %d after PgUp at top, want 0", m.detail.scroll)
	}
}

// TestScenario_ReadAnIssue_ArrowKeysScrollOnShortTerminal pins the
// fix for the original "↑↓ scrolling doesn't work on short terminals"
// bug: every issue has a synthetic issue.created event, so the legacy
// handleUp/handleDown logic kept ↑/↓ on the activity-tab cursor and
// never reached the body. With the unified-viewport refactor, ↑/↓ is
// always document scroll — on a short terminal full of body the user
// can read past the visible window without learning PgUp/PgDn.
func TestScenario_ReadAnIssue_ArrowKeysScrollOnShortTerminal(t *testing.T) {
	body := strings.Repeat("scrollable line\n", 80) + "ARROW_TAIL_MARKER"
	m := setupDetailScenario(t, 120, 24, body)
	assertViewMissing(t, m, "ARROW_TAIL_MARKER")
	m = pressN(t, m, tea.KeyPressMsg{Code: tea.KeyDown}, 80)
	assertViewContains(t, m, "ARROW_TAIL_MARKER")
}

// TestScenario_NavigateTabs_TabAdvancesActiveSection drives Tab and
// asserts the active-tab bracket follows. Catches a class of bug
// where Tab gets silently swallowed (e.g. eaten by an open modal,
// double-bound, or clobbered by a global handler), which a snapshot
// of one focus state can't see.
func TestScenario_NavigateTabs_TabAdvancesActiveSection(t *testing.T) {
	m := setupDetailScenario(t, 120, 30, "short body")
	assertViewContains(t, m, "[ Comments (1) ]")
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	assertViewContains(t, m, "[ Events (1) ]")
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	assertViewContains(t, m, "[ Links (0) ]")
}

// TestScenario_EscReturnsFromStackedDetailToList drives the same path
// Bubble Tea uses in the real TUI: Esc reaches the detail view, which
// returns a popDetailMsg command, and the command is then fed back into
// Model.Update. A submodel-only test can miss this parent handoff.
func TestScenario_EscReturnsFromStackedDetailToList(t *testing.T) {
	m := setupDetailScenario(t, 120, 30, "short body")
	if m.view != viewDetail {
		t.Fatalf("setup: view=%v, want viewDetail", m.view)
	}
	m = sendKeyAndDrain(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.view != viewList {
		t.Fatalf("Esc did not return to list: view=%v, want viewList", m.view)
	}
}

// TestScenario_EscReturnsFromSplitDetailToList pins the split-pane
// parent state as well as focus. Enter opens/focuses detail in split
// mode; Esc should make the list the active view again so future
// overlays, layout flips, and model-level gates agree with what the
// user just did.
func TestScenario_EscReturnsFromSplitDetailToList(t *testing.T) {
	m := setupDetailScenario(t, 200, 40, "short body")
	assertViewState(t, m, layoutSplit, viewDetail, focusDetail)
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.focus != focusList {
		t.Fatalf("Esc did not return focus to list: focus=%v", m.focus)
	}
	if m.view != viewList {
		t.Fatalf("Esc left stale active view: view=%v, want viewList", m.view)
	}
}

// TestScenario_EscPopsSplitDetailNavStackBeforeLeavingPane covers a
// nested detail jump in split mode. Esc must first unwind the detail
// nav stack, matching Backspace and stacked detail behavior; only a
// second back action should leave the detail pane.
func TestScenario_EscPopsSplitDetailNavStackBeforeLeavingPane(t *testing.T) {
	m := setupDetailScenario(t, 200, 40, "parent body")
	parent := m.detail
	child := Issue{ProjectID: 7, UID: "01TEST-c002", ShortID: "c002", Title: "child task", Status: "open"}
	m.detail = detailModel{
		issue:    &child,
		scopePID: 7,
		gen:      parent.gen + 1,
		navStack: []detailModel{
			parent,
		},
	}
	m.focus = focusDetail
	m.view = viewDetail

	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.focus != focusDetail {
		t.Fatalf("Esc with nav stack moved focus=%v, want focusDetail", m.focus)
	}
	if m.detail.issue == nil || m.detail.issue.ShortID != "aaa1" {
		t.Fatalf("Esc did not pop to parent detail: issue=%+v", m.detail.issue)
	}
}

// TestScenario_BackspaceReturnsFromSplitDetailToList exercises the
// other key advertised by "esc/backspace back". Backspace reaches the
// detail model first, then emits popDetailMsg; the parent handler must
// clear split focus as well as view state.
func TestScenario_BackspaceReturnsFromSplitDetailToList(t *testing.T) {
	m := setupDetailScenario(t, 200, 40, "short body")
	assertViewState(t, m, layoutSplit, viewDetail, focusDetail)
	m = sendKeyAndDrain(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.view != viewList {
		t.Fatalf("Backspace did not return view to list: view=%v", m.view)
	}
	if m.focus != focusList {
		t.Fatalf("Backspace did not return focus to list: focus=%v", m.focus)
	}
}

// TestScenario_LayoutToggle_LFlipsAtSplitEligibleSize: at a size that
// triggers split layout, pressing L flips to stacked. Catches the L
// keymap conflict (pre-93e37ec, L in detail opened the link prompt
// instead of toggling).
func TestScenario_LayoutToggle_LFlipsAtSplitEligibleSize(t *testing.T) {
	m := scenarioModel(t, 200, 40)
	if m.layout != layoutSplit {
		t.Fatalf("setup: layout=%v, want layoutSplit at 200x40", m.layout)
	}
	m = pressRune(t, m, 'L')
	if m.layout != layoutStacked {
		t.Fatalf("L did not flip to stacked: layout=%v", m.layout)
	}
	if !m.layoutLocked {
		t.Error("layoutLocked = false after L; user toggle must stick")
	}
}

func TestScenario_ChildSortToggle_OFlipsBetweenTopologicalAndTemporal(t *testing.T) {
	m := scenarioModel(t, 120, 30)
	if m.list.childSort != childSortTopological {
		t.Fatalf("default child sort = %v, want topological", m.list.childSort)
	}

	m = pressRune(t, m, 'o')
	if m.list.childSort != childSortTemporal {
		t.Fatalf("first o child sort = %v, want temporal", m.list.childSort)
	}

	m = pressRune(t, m, 'o')
	if m.list.childSort != childSortTopological {
		t.Fatalf("second o child sort = %v, want topological", m.list.childSort)
	}
}

// TestScenario_LayoutToggle_LStaysAcrossResize: once the user has
// pressed L, a subsequent WindowSizeMsg cannot revert the layout via
// pickLayout. Without the lock, "I pinned stacked, then resized" would
// silently auto-flip back to split.
func TestScenario_LayoutToggle_LStaysAcrossResize(t *testing.T) {
	m := scenarioModel(t, 200, 40)
	m = pressRune(t, m, 'L')
	if m.layout != layoutStacked {
		t.Fatalf("setup: L did not flip to stacked: layout=%v", m.layout)
	}
	out, _ := m.Update(tea.WindowSizeMsg{Width: 220, Height: 50})
	m = out.(Model)
	if m.layout != layoutStacked {
		t.Fatalf("resize reverted layout: %v, want layoutStacked (locked)", m.layout)
	}
}

// TestScenario_LayoutToggle_NarrowThenWidePreservesSplitIntent: the
// regression for roborev #17173 finding 1. After locking split and
// resizing too narrow to render it, the rendered layout degrades to
// stacked — but the user's split intent must survive: resizing wide
// again must return to split. Before the fix, m.layout was stomped
// with the degraded value and the split preference vanished, so the
// terminal staying wide forever after still rendered stacked.
func TestScenario_LayoutToggle_NarrowThenWidePreservesSplitIntent(t *testing.T) {
	m := scenarioModel(t, 200, 40)
	if m.layout != layoutSplit {
		t.Fatalf("setup: layout=%v, want layoutSplit at 200x40", m.layout)
	}
	// Lock split via two L presses (split → stacked → split, locked).
	m = pressRune(t, m, 'L')
	m = pressRune(t, m, 'L')
	if m.layout != layoutSplit || !m.layoutLocked {
		t.Fatalf("setup: layout=%v locked=%v, want layoutSplit + locked",
			m.layout, m.layoutLocked)
	}
	// Resize too narrow for split — rendered must degrade to stacked.
	out, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = out.(Model)
	if m.layout != layoutStacked {
		t.Fatalf("narrow resize: layout=%v, want layoutStacked (degraded)",
			m.layout)
	}
	// Resize wide again — rendered must return to split because the
	// user's preference was never lost.
	out, _ = m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
	m = out.(Model)
	if m.layout != layoutSplit {
		t.Fatalf("wide resize: layout=%v, want layoutSplit (preference restored)",
			m.layout)
	}
}

// TestScenario_LDoesNotOpenLinkPromptInDetail: regression for the
// L→AddLink keymap collision. AddLink moved to lowercase l in
// 93e37ec; capital L is the layout toggle and must not surface the
// link-input prompt. Two assertions are independent — a regression
// that flips one but not both must still fail (roborev #17192
// finding 1).
func TestScenario_LDoesNotOpenLinkPromptInDetail(t *testing.T) {
	m := setupDetailScenario(t, 200, 40, "short body")
	prevLayout := m.layout
	m = pressRune(t, m, 'L')
	if m.input.kind != inputNone {
		t.Fatalf("L opened input %v in detail; want layout toggle, not link prompt", m.input.kind)
	}
	if m.layout == prevLayout {
		t.Errorf("L did not toggle layout (was %v, still %v)", prevLayout, m.layout)
	}
	if !m.layoutLocked {
		t.Errorf("L press did not set layoutLocked")
	}
}

// TestScenario_LowercaseLOpensLinkPromptInDetail: the matching
// positive path — lowercase l should still open the link prompt. If
// this test fails, AddLink got accidentally rebound or removed.
func TestScenario_LowercaseLOpensLinkPromptInDetail(t *testing.T) {
	m := setupDetailScenario(t, 120, 30, "short body")
	m = sendKeyAndDrain(t, m, runeKey('l'))
	if m.input.kind != inputLinkPrompt {
		t.Fatalf("lowercase l did not open link prompt; m.input.kind = %v", m.input.kind)
	}
}

// sendKeyAndDrain dispatches a KeyMsg and then drains a single
// follow-up Cmd, threading the resulting Msg back through Update.
// Detail-mode mutation keys (l/c/+/p/...) emit openInputCmd, which
// produces an openInputMsg consumed by Model.Update — so a one-step
// drain is enough to observe m.input.kind change in the same test
// iteration. Stops if the cmd is nil OR the cmd's first message is
// not consumed (e.g. a tea.Batch — the test doesn't try to
// recursively unwrap; flag it as a sign the helper needs extending).
func sendKeyAndDrain(t *testing.T, m Model, msg tea.KeyPressMsg) Model {
	t.Helper()
	out, cmd := m.Update(msg)
	m = out.(Model)
	if cmd == nil {
		return m
	}
	produced := cmd()
	if produced == nil {
		return m
	}
	out, _ = m.Update(produced)
	return out.(Model)
}

// TestScenario_ChildCreatedRefetchesParentDetail: the SSE
// regression I fixed in d8c8a3c. With the parent open, an
// issue.created event whose payload carries a parent link must
// trigger a parent-detail refetch — without it, the children
// section sits stale until the user reloads.
//
// maybeRefetchOpenDetail short-circuits when m.api is nil (it can't
// build a fetch cmd without a client), so we hand it a stub *Client.
// The cmd it returns isn't invoked — we just assert it was produced,
// which proves the SSE → detail-refetch routing is intact.
func TestScenario_ChildCreatedRefetchesParentDetail(t *testing.T) {
	m := scenarioModel(t, 120, 30)
	m.api = NewClient("http://kata.invalid", nil)
	m = scenarioOpenDetail(t, m, "short body")
	parentSID := m.detail.issue.ShortID
	parentUID := m.detail.issue.UID
	cmd := m.maybeRefetchOpenDetail(eventReceivedMsg{
		eventType:    "issue.created",
		projectID:    m.detail.scopePID,
		issueShortID: "k9k9",
		issueUID:     "01TEST-k9k9",
		link: &linkPayload{
			Type:         "parent",
			FromShortID:  "k9k9",
			FromIssueUID: "01TEST-k9k9",
			ToShortID:    parentSID,
			ToIssueUID:   parentUID,
		},
	})
	if cmd == nil {
		t.Fatal("issue.created with parent link did not dispatch parent-detail refetch")
	}
}

// TestScenario_BodyScrollWorksWhileChildrenFocused covers the
// children-focus + long body case. Even when the cursor is parked
// on the children list, PageDown should still advance the body
// scroll — the body-scroll keys are intentionally focus-agnostic.
func TestScenario_BodyScrollWorksWhileChildrenFocused(t *testing.T) {
	body := strings.Repeat("scrollable line\n", 60) + "BODY_TAIL"
	m := setupDetailScenario(t, 120, 30, body)
	// Seed a child so children focus is reachable.
	m.detail.children = []Issue{{UID: "01TEST-c099", ShortID: "c099", Title: "child", Status: "open"}}
	// Cycle into children focus via Tab. The detail focus order is
	// Children → Comments → Events → Links → Children, so one Tab
	// from Comments gets us to Events, and Shift+Tab from Comments
	// goes to Children. We use Shift+Tab so the test doesn't
	// depend on which tab cycleDetailFocus(1) lands on.
	m = pressKey(t, m, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.detail.detailFocus != focusChildren {
		t.Fatalf("setup: focus = %v, want focusChildren", m.detail.detailFocus)
	}
	m = pressN(t, m, tea.KeyPressMsg{Code: tea.KeyPgDown}, 16)
	assertViewContains(t, m, "BODY_TAIL")
}
