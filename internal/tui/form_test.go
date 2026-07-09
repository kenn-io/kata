package tui

import (
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// captureCreateIssue stands up a test client whose handler records the
// request path and replies with a minimal successful CreateIssue
// payload. Returns the client and a pointer the caller dereferences
// after the dispatch to inspect the request URL.
func captureCreateIssue(t *testing.T) (*Client, *string) {
	t.Helper()
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respondJSON(t, w, map[string]any{
			"issue":   map[string]any{"number": 1, "title": "hi", "status": "open"},
			"changed": true,
		})
	})
	return c, &gotPath
}

// unwrapMutationCmd evaluates cmd, asserts the result is a
// mutationDoneMsg with no err, and returns it. Fails the test with a
// useful message on any mismatch.
func unwrapMutationCmd(t *testing.T, cmd tea.Cmd) mutationDoneMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected mutation cmd, got nil")
	}
	msg := cmd()
	mut, ok := msg.(mutationDoneMsg)
	if !ok {
		t.Fatalf("cmd returned %T, want mutationDoneMsg", msg)
	}
	return mut
}

// errStub is a minimal error type for tests that need a non-nil error
// of a known string. Re-introduced here after the editor-test cleanup
// in commit 2 removed the original detail_editor_test.go declaration.
type errStub string

func (e errStub) Error() string { return string(e) }

// TestDetail_EKey_OpensBodyEditForm: pressing `e` from the detail
// view opens the centered body editor pre-filled with the issue
// body. Replaces the old shell-out-to-$EDITOR path.
func TestDetail_EKey_OpensBodyEditForm(t *testing.T) {
	m := formFixture()
	m.view = viewDetail
	out, _ := m.Update(runeKey('e'))
	nm := out.(Model)
	if nm.input.kind != inputBodyEditForm {
		t.Fatalf("e did not open body edit form; kind = %v", nm.input.kind)
	}
	if got := nm.input.activeField().value(); got != "current body" {
		t.Fatalf("textarea = %q, want %q (pre-fill from issue)",
			got, "current body")
	}
}

// TestDetail_CKey_OpensCommentForm: pressing `c` from the detail
// view opens the centered comment editor empty. Replaces the old
// shell-out-to-$EDITOR path.
func TestDetail_CKey_OpensCommentForm(t *testing.T) {
	m := formFixture()
	m.view = viewDetail
	out, _ := m.Update(runeKey('c'))
	nm := out.(Model)
	if nm.input.kind != inputCommentForm {
		t.Fatalf("c did not open comment form; kind = %v", nm.input.kind)
	}
	if got := nm.input.activeField().value(); got != "" {
		t.Fatalf("textarea = %q, want empty", got)
	}
}

// TestDetail_FormKeys_NoIssue_NoOp: `e`/`c` while no issue is open
// (loading state) must not open a form.
func TestDetail_FormKeys_NoIssue_NoOp(t *testing.T) {
	m := formFixture()
	m.view = viewDetail
	m.detail.issue = nil
	for _, k := range []rune{'e', 'c'} {
		out, _ := m.Update(runeKey(k))
		nm := out.(Model)
		if nm.input.kind != inputNone {
			t.Fatalf("%q opened a form with no issue: %v", k, nm.input.kind)
		}
	}
}

// TestRouteFormMutation_Success_ClosesFormAndDispatchesToDetail: a
// successful form-side mutationDoneMsg closes the form (input clears)
// and re-classifies as origin=detail so the existing detail
// applyMutation logic refreshes the body / comments.
func TestRouteFormMutation_Success_ClosesFormAndDispatchesToDetail(t *testing.T) {
	m := formFixture()
	m.view = viewDetail
	m = m.openBodyEditForm()
	m.input.saving = true // simulate in-flight
	mut := mutationDoneMsg{
		origin: "form", kind: "form.body.edit", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa", Body: "new body"}},
	}
	out, _ := m.Update(mut)
	nm := out.(Model)
	if nm.input.kind != inputNone {
		t.Fatalf("form did not close on success; kind = %v", nm.input.kind)
	}
}

// TestRouteFormMutation_Error_KeepsFormAndShowsError: a form-side
// mutation error leaves the form open and surfaces the error on
// the form's err line; saving=false so the user can retry.
func TestRouteFormMutation_Error_KeepsFormAndShowsError(t *testing.T) {
	m := formFixture()
	m.view = viewDetail
	m = m.openBodyEditForm()
	m.input.saving = true
	mut := mutationDoneMsg{
		origin: "form", kind: "form.body.edit", formGen: m.input.formGen,
		err: errStub("daemon 500"),
	}
	out, _ := m.Update(mut)
	nm := out.(Model)
	if nm.input.kind != inputBodyEditForm {
		t.Fatalf("form closed on error; kind = %v", nm.input.kind)
	}
	if nm.input.saving {
		t.Fatal("saving flag stayed true after error; user can't retry")
	}
	if !strings.Contains(nm.input.err, "daemon 500") {
		t.Fatalf("err = %q, want it to contain the error message", nm.input.err)
	}
}

// formFixture returns a Model with an open detail issue so the M4
// form-open helpers can resolve a target. issueNumber=42 / projectID=7
// matches the existing test fixtures.
func formFixture() Model {
	return Model{
		view:   viewList,
		keymap: newKeymap(),
		list:   listModel{actor: "tester"},
		scope:  scope{projectID: 7, projectName: "kata"},
		detail: detailModel{
			issue:    &Issue{UID: "01TEST-42aa", ShortID: "42aa", Title: "fix bug", Body: "current body"},
			scopePID: 7,
			gen:      1,
		},
		cache: newIssueCache(),
	}
}

// TestForm_OpenBodyEditForm_AllocatesFreshFormGen: every open
// increments nextFormGen so a stale handoff from a prior form is
// rejected. Reopening an edit form must produce a higher formGen.
func TestForm_OpenBodyEditForm_AllocatesFreshFormGen(t *testing.T) {
	m := formFixture()
	m1 := m.openBodyEditForm()
	if m1.input.kind != inputBodyEditForm {
		t.Fatalf("kind = %v, want inputBodyEditForm", m1.input.kind)
	}
	if m1.input.formGen == 0 {
		t.Fatal("formGen = 0; want monotonic positive")
	}
	if m1.input.target.issueShortID != "42aa" {
		t.Fatalf("target issueShortID = %q, want 42aa", m1.input.target.issueShortID)
	}
	if got := m1.input.activeField().value(); got != "current body" {
		t.Fatalf("textarea = %q, want %q (pre-fill from issue body)",
			got, "current body")
	}
	m2 := m1.openBodyEditForm()
	if m2.input.formGen <= m1.input.formGen {
		t.Fatalf("formGen did not advance: %d -> %d",
			m1.input.formGen, m2.input.formGen)
	}
}

// TestForm_OpenCommentForm_StartsEmpty: comment form opens with an
// empty textarea (no template), pre-allocated formGen, target locked
// to the open issue.
func TestForm_OpenCommentForm_StartsEmpty(t *testing.T) {
	m := formFixture()
	m1 := m.openCommentForm()
	if m1.input.kind != inputCommentForm {
		t.Fatalf("kind = %v, want inputCommentForm", m1.input.kind)
	}
	if got := m1.input.activeField().value(); got != "" {
		t.Fatalf("textarea = %q, want empty", got)
	}
	if m1.input.target.issueShortID != "42aa" {
		t.Fatalf("target issueShortID = %q, want 42aa", m1.input.target.issueShortID)
	}
}

// TestForm_CtrlS_OnEmptyComment_BlocksAndShowsError: ctrl+s on an
// empty comment form must NOT dispatch AddComment; the form stays
// open with an error message. Distinguishes from body edit, where
// empty content is legitimate.
func TestForm_CtrlS_OnEmptyComment_BlocksAndShowsError(t *testing.T) {
	m := formFixture()
	m = m.openCommentForm()
	out, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	nm := out.(Model)
	if cmd != nil {
		t.Fatalf("empty comment dispatched cmd %T; want nil", cmd)
	}
	if nm.input.kind != inputCommentForm {
		t.Fatalf("form closed on empty commit; kind = %v", nm.input.kind)
	}
	if nm.input.err == "" {
		t.Fatal("expected in-form error on empty commit")
	}
}

// TestForm_CtrlS_OnEmptyBodyEdit_AllowedToCommit: clearing a body
// is legitimate; ctrl+s on an empty body edit form must dispatch
// EditBody with body="" — no in-form error.
func TestForm_CtrlS_OnEmptyBodyEdit_AllowedToCommit(t *testing.T) {
	m := formFixture()
	m = m.openBodyEditForm()
	// Clear the textarea (it pre-filled with the existing body).
	m.input.activeField().setValue("")
	m.input.fields[m.input.active] = *m.input.activeField()
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("empty body edit must dispatch EditBody (clearing is legitimate)")
	}
}

// TestForm_CtrlS_SetsSavingGate: the first ctrl+s flips saving=true
// and a duplicate ctrl+s while saving is absorbed (no second cmd).
// Regression for the "duplicate ctrl+s issues two mutations" race.
func TestForm_CtrlS_SetsSavingGate(t *testing.T) {
	m := formFixture()
	m = m.openCommentForm()
	m.input.activeField().setValue("hello")
	m.input.fields[m.input.active] = *m.input.activeField()
	out, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	nm := out.(Model)
	if cmd == nil {
		t.Fatal("first ctrl+s should dispatch AddComment")
	}
	if !nm.input.saving {
		t.Fatal("saving flag not set after first ctrl+s")
	}
	// Duplicate ctrl+s while saving must be a no-op.
	out2, cmd2 := nm.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	nm2 := out2.(Model)
	if cmd2 != nil {
		t.Fatalf("duplicate ctrl+s dispatched cmd %T; want nil (saving gate)", cmd2)
	}
	if !nm2.input.saving {
		t.Fatal("saving flag cleared by duplicate ctrl+s; should still be set")
	}
}

// TestForm_Esc_CancelsAndClosesForm: esc on any form kind closes
// the form without dispatching a mutation.
func TestForm_Esc_CancelsAndClosesForm(t *testing.T) {
	m := formFixture()
	m = m.openCommentForm()
	m.input.activeField().setValue("draft comment")
	out, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	nm := out.(Model)
	if nm.input.kind != inputNone {
		t.Fatalf("esc did not close form; kind = %v", nm.input.kind)
	}
	if cmd != nil {
		// No mutation cmd; cancel cmd path is reserved for the post-create
		// detail-open in commit 2.
		if msg := cmd(); msg != nil {
			if _, isMut := msg.(mutationDoneMsg); isMut {
				t.Fatalf("esc dispatched mutation: %v", msg)
			}
		}
	}
}

// TestForm_CtrlE_RequestsEditorHandoff_TaggedWithFormGen: ctrl+e on
// a form returns an editor cmd. The editor cmd is opaque to the
// test (it shells out to $EDITOR) but the form's formGen rides
// through editorCmd into the eventual editorReturnedMsg, which we
// validate via the return-routing tests below.
func TestForm_CtrlE_RequestsEditorHandoff_TaggedWithFormGen(t *testing.T) {
	m := formFixture()
	m = m.openBodyEditForm()
	formGenAtOpen := m.input.formGen
	out, cmd := m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	nm := out.(Model)
	if cmd == nil {
		t.Fatal("ctrl+e produced no cmd; expected editor handoff")
	}
	// Form must stay open during the editor session — the writeback
	// will re-populate the textarea on return.
	if nm.input.kind != inputBodyEditForm {
		t.Fatalf("form closed on ctrl+e; kind = %v", nm.input.kind)
	}
	if nm.input.formGen != formGenAtOpen {
		t.Fatalf("formGen mutated by ctrl+e: %d -> %d",
			formGenAtOpen, nm.input.formGen)
	}
}

// TestForm_EditorReturn_MatchingFormGen_WritesBackToTextarea: a
// successful editor return whose formGen matches the active form
// repopulates the textarea with the returned content.
func TestForm_EditorReturn_MatchingFormGen_WritesBackToTextarea(t *testing.T) {
	m := formFixture()
	m = m.openBodyEditForm()
	gen := m.input.formGen
	out, _ := m.Update(editorReturnedMsg{
		kind: "edit", content: "edited via vim", formGen: gen,
	})
	nm := out.(Model)
	if got := nm.input.activeField().value(); got != "edited via vim" {
		t.Fatalf("textarea = %q, want %q (writeback)", got, "edited via vim")
	}
	if nm.input.err != "" {
		t.Fatalf("err = %q, want empty after successful return", nm.input.err)
	}
}

// TestForm_EditorReturn_StaleFormGen_DropsContent: an editor return
// whose formGen doesn't match the currently-open form (because the
// form was closed and re-opened in the meantime) is silently
// discarded. The current form's textarea is unchanged. Regression
// for the stale-handoff race class.
func TestForm_EditorReturn_StaleFormGen_DropsContent(t *testing.T) {
	m := formFixture()
	m = m.openCommentForm()
	staleGen := m.input.formGen
	// Close (esc) and re-open — new formGen.
	out, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = out.(Model)
	m = m.openCommentForm()
	m.input.activeField().setValue("typed in fresh form")
	m.input.fields[m.input.active] = *m.input.activeField()
	freshGen := m.input.formGen
	if freshGen == staleGen {
		t.Fatal("formGen did not advance between forms; test is moot")
	}
	// Stale return arrives, tagged with the old formGen.
	out, _ = m.Update(editorReturnedMsg{
		kind: "comment", content: "stale content from old editor", formGen: staleGen,
	})
	nm := out.(Model)
	if got := nm.input.activeField().value(); got != "typed in fresh form" {
		t.Fatalf("textarea = %q, want %q (stale return must not write)",
			got, "typed in fresh form")
	}
}

// TestForm_EditorReturn_NoActiveForm_DropsContent: editor return
// arrives with formGen=N but no form is active. Must not panic, must
// not change anything.
func TestForm_EditorReturn_NoActiveForm_DropsContent(t *testing.T) {
	m := formFixture()
	out, cmd := m.Update(editorReturnedMsg{
		kind: "edit", content: "x", formGen: 999,
	})
	nm := out.(Model)
	if nm.input.kind != inputNone {
		t.Fatalf("orphan return opened a form: %v", nm.input.kind)
	}
	if cmd != nil {
		t.Fatalf("orphan return dispatched cmd %T", cmd)
	}
}

// TestForm_EditorReturn_WithError_KeepsBufferAndShowsStatus: the
// editor exited with an error (non-zero exit, tmpfile read fail).
// The form stays open with its previous buffer intact and the err
// surfaces on the form's status line.
func TestForm_EditorReturn_WithError_KeepsBufferAndShowsStatus(t *testing.T) {
	m := formFixture()
	m = m.openBodyEditForm()
	gen := m.input.formGen
	originalBuffer := m.input.activeField().value()
	out, _ := m.Update(editorReturnedMsg{
		kind: "edit", err: errStub("editor exit 1"), formGen: gen,
	})
	nm := out.(Model)
	if got := nm.input.activeField().value(); got != originalBuffer {
		t.Fatalf("textarea changed on editor error: %q vs %q",
			got, originalBuffer)
	}
	if !strings.Contains(nm.input.err, "editor") {
		t.Fatalf("err = %q, want editor-prefixed message", nm.input.err)
	}
}

// TestForm_LegacyEditorReturn_FormGenZero_DoesNotRouteToFormPath:
// editor returns with formGen=0 are the legacy detail-side shell-
// out path and must NOT touch m.input.
func TestForm_LegacyEditorReturn_FormGenZero_DoesNotRouteToFormPath(t *testing.T) {
	m := formFixture()
	m = m.openBodyEditForm()
	originalBuffer := m.input.activeField().value()
	// formGen=0 → must skip routeEditorReturn entirely.
	_, _ = m.Update(editorReturnedMsg{
		kind: "edit", content: "should not reach form", formGen: 0,
	})
	// The form's buffer is unchanged because the return wasn't routed
	// to the form path.
	if got := m.input.activeField().value(); got != originalBuffer {
		t.Fatalf("legacy return wrote into form: %q", got)
	}
}

// TestOpenBodyEditForm_UsesDetailScopePID pins that the form built
// when pressing 'e' on a detail opened from all-projects scope
// targets the issue's actual project (m.detail.scopePID), not
// m.scope.projectID — which is 0 in all-projects scope. Without
// this, save dispatches would post to /api/v1/projects/0/...
func TestOpenBodyEditForm_UsesDetailScopePID(t *testing.T) {
	m := formFixture()
	m.scope = scope{allProjects: true} // m.scope.projectID == 0
	m.detail.scopePID = 42
	m.detail.issue = &Issue{UID: "01TEST-7bb", ShortID: "7bb", Body: "old"}

	out := m.openBodyEditForm()
	if out.input.kind != inputBodyEditForm {
		t.Fatalf("input kind = %v, want inputBodyEditForm", out.input.kind)
	}
	if got := out.input.target.projectID; got != 42 {
		t.Fatalf("target.projectID = %d, want 42 (m.detail.scopePID)", got)
	}
	if got := out.input.target.issueShortID; got != "7bb" {
		t.Fatalf("target.issueShortID = %q, want 7bb", got)
	}
}

// TestOpenCommentForm_UsesDetailScopePID — same pattern for the
// comment form opened by 'c' on a detail in all-projects scope.
func TestOpenCommentForm_UsesDetailScopePID(t *testing.T) {
	m := formFixture()
	m.scope = scope{allProjects: true}
	m.detail.scopePID = 42
	m.detail.issue = &Issue{UID: "01TEST-7bb", ShortID: "7bb"}

	out := m.openCommentForm()
	if out.input.kind != inputCommentForm {
		t.Fatalf("input kind = %v, want inputCommentForm", out.input.kind)
	}
	if got := out.input.target.projectID; got != 42 {
		t.Fatalf("target.projectID = %d, want 42 (m.detail.scopePID)", got)
	}
	if got := out.input.target.issueShortID; got != "7bb" {
		t.Fatalf("target.issueShortID = %q, want 7bb", got)
	}
}

// TestCommitNewIssueForm_SplitLayoutFocusDetail_UsesDetailScopePID
// pins the split-layout fix: when the user presses N from a detail
// pane that's focused in split mode (m.layout == layoutSplit,
// m.focus == focusDetail), m.view is still viewList. The earlier
// guard switched to m.detail.scopePID only when m.view == viewDetail,
// which missed this case and dispatched a CreateIssue against
// m.scope.projectID == 0 in all-projects scope. The fix uses
// detailIsActive() so split + focusDetail also routes through
// m.detail.scopePID. Regression for roborev job 17575.
func TestCommitNewIssueForm_SplitLayoutFocusDetail_UsesDetailScopePID(t *testing.T) {
	api, gotPath := captureCreateIssue(t)
	m := Model{
		api:    api,
		view:   viewList,
		layout: layoutSplit,
		focus:  focusDetail,
		keymap: newKeymap(),
		list:   listModel{actor: "tester"},
		scope:  scope{allProjects: true},
		detail: detailModel{
			issue:    &Issue{UID: "01TEST-7bb", ShortID: "7bb"},
			scopePID: 42,
		},
		cache: newIssueCache(),
	}
	if !m.detailIsActive() {
		t.Fatalf("setup: detailIsActive=false in split+focusDetail; test would not exercise the gate")
	}
	m, _ = m.openInput(inputNewIssueForm)
	if m.input.kind != inputNewIssueForm {
		t.Fatalf("openInput did not produce inputNewIssueForm; got %v", m.input.kind)
	}
	m.input.fields[0].input.SetValue("hi")

	_, cmd := m.commitNewIssueForm()
	mut := unwrapMutationCmd(t, cmd)
	if mut.err != nil {
		t.Fatalf("CreateIssue dispatch failed: %v (path=%q)", mut.err, *gotPath)
	}
	if !strings.Contains(*gotPath, "/api/v1/projects/42/issues") {
		t.Fatalf("URL path = %q, want /api/v1/projects/42/issues "+
			"(split+focusDetail must use m.detail.scopePID, not m.scope.projectID=0)",
			*gotPath)
	}
}

// TestCommitNewIssueForm_StackedViewDetail_UsesDetailScopePID pins
// the original (pre-split) contract: from stacked viewDetail in
// all-projects scope, the create-issue dispatch targets the issue's
// project (m.detail.scopePID), not m.scope.projectID==0. The
// detailIsActive() switch must keep this path working.
func TestCommitNewIssueForm_StackedViewDetail_UsesDetailScopePID(t *testing.T) {
	api, gotPath := captureCreateIssue(t)
	m := Model{
		api:    api,
		view:   viewDetail,
		layout: layoutStacked,
		keymap: newKeymap(),
		list:   listModel{actor: "tester"},
		scope:  scope{allProjects: true},
		detail: detailModel{
			issue:    &Issue{UID: "01TEST-7bb", ShortID: "7bb"},
			scopePID: 42,
		},
		cache: newIssueCache(),
	}
	m, _ = m.openInput(inputNewIssueForm)
	m.input.fields[0].input.SetValue("hi")

	_, cmd := m.commitNewIssueForm()
	mut := unwrapMutationCmd(t, cmd)
	if mut.err != nil {
		t.Fatalf("CreateIssue dispatch failed: %v (path=%q)", mut.err, *gotPath)
	}
	if !strings.Contains(*gotPath, "/api/v1/projects/42/issues") {
		t.Fatalf("URL path = %q, want /api/v1/projects/42/issues", *gotPath)
	}
}
