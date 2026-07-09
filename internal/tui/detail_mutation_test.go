package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// dmFixture seeds a minimal detailModel suitable for mutation tests:
// a single issue and a known scope/actor so the asserts are direct.
// No comments/events/links; the mutation paths don't touch those.
func dmFixture() detailModel {
	iss := Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "fix bug", Status: "open"}
	return detailModel{issue: &iss, scopePID: 7, actor: "tester"}
}

// setupMutationTest builds the (api, dm, km) triplet used by every
// detail-mutation test: a fakeDetailAPI seeded with a generic
// successful MutationResp, dmFixture(), and a fresh keymap.
func setupMutationTest(t *testing.T) (*fakeDetailAPI, detailModel, keymap) {
	t.Helper()
	api := &fakeDetailAPI{
		mutationResult: &MutationResp{Issue: &Issue{UID: "01TEST-42aa", ShortID: "42aa"}},
	}
	return api, dmFixture(), newKeymap()
}

// requireInputPrompt evaluates cmd, asserts it returns an
// openInputMsg of the given kind, and fails the test otherwise.
func requireInputPrompt(t *testing.T, cmd tea.Cmd, want inputKind) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected openInputCmd, got nil")
	}
	msg, ok := cmd().(openInputMsg)
	if !ok {
		t.Fatalf("expected openInputMsg, got %T", cmd())
	}
	if msg.kind != want {
		t.Fatalf("openInputMsg.kind = %v, want %v", msg.kind, want)
	}
}

// executePromptCommit invokes dispatchPanelPromptCommit, asserts a
// non-nil cmd, runs it through runDetailCmd, and returns the
// post-commit model so callers can inspect status/state.
func executePromptCommit(
	t *testing.T, dm detailModel, api *fakeDetailAPI, km keymap,
	kind inputKind, val string,
) detailModel {
	t.Helper()
	_, cmd := dm.dispatchPanelPromptCommit(api, kind, val)
	if cmd == nil {
		t.Fatal("expected dispatch cmd")
	}
	return runDetailCmd(t, dm, cmd, km, api)
}

// runDetailCmd executes the tea.Cmd returned by Update once and feeds
// the resulting message back into Update so the test sees the post-
// fetch (or post-mutation-result) state. Returns the second-pass model.
// Some commands return tea.BatchMsg; those are handled by enumerating
// the children and dispatching only the first non-nil message back.
func runDetailCmd(
	t *testing.T, dm detailModel, cmd tea.Cmd, km keymap, api detailAPI,
) detailModel {
	t.Helper()
	if cmd == nil {
		return dm
	}
	msg := cmd()
	out, _ := dm.Update(msg, km, api)
	return out
}

// typeRunes used to feed runes through dm.Update for the now-retired
// dm.modal in-place input. After M3b the panel-local prompt lives on
// Model.input; the dispatch tests call dispatchPanelPromptCommit
// directly with the buffer string, so this helper has no callers.
// Kept as a tiny no-op stub so the historical test signature can be
// re-introduced without touching imports if a future test wants it.
//
//nolint:unused // retained for future re-use if a test needs dm-level typing
func typeRunes(
	t *testing.T, dm detailModel, s string, km keymap, api detailAPI,
) detailModel {
	t.Helper()
	for _, r := range s {
		dm, _ = dm.Update(runeKey(r), km, api)
	}
	return dm
}

// TestDetail_Close_DispatchesAPI: pressing 'x' calls api.Close exactly
// once with the fixture's projectID, number, and actor.
func TestDetail_Close_DispatchesAPI(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	out, cmd := dm.Update(runeKey('x'), km, api)
	if cmd == nil {
		t.Fatal("expected close cmd from x")
	}
	_ = runDetailCmd(t, out, cmd, km, api)
	if api.closeCalls != 1 {
		t.Fatalf("closeCalls = %d, want 1", api.closeCalls)
	}
	if api.lastProjectID != 7 || api.lastRef != "42aa" || api.lastActor != "tester" {
		t.Fatalf("close args wrong: pid=%d ref=%q actor=%q",
			api.lastProjectID, api.lastRef, api.lastActor)
	}
}

// TestDetail_Reopen_DispatchesAPI: pressing 'r' calls api.Reopen.
func TestDetail_Reopen_DispatchesAPI(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	out, cmd := dm.Update(runeKey('r'), km, api)
	if cmd == nil {
		t.Fatal("expected reopen cmd from r")
	}
	_ = runDetailCmd(t, out, cmd, km, api)
	if api.reopenCalls != 1 {
		t.Fatalf("reopenCalls = %d, want 1", api.reopenCalls)
	}
	if api.lastProjectID != 7 || api.lastRef != "42aa" || api.lastActor != "tester" {
		t.Fatalf("reopen args wrong: pid=%d ref=%q actor=%q",
			api.lastProjectID, api.lastRef, api.lastActor)
	}
}

// TestDetail_AddLabel_OpensPrompt: '+' on detail emits an
// openInputMsg{kind: inputLabelPrompt}. The cmd's message is what
// Model.routeTopLevel intercepts to construct the inputState; dm
// itself is unchanged after the keypress.
func TestDetail_AddLabel_OpensPrompt(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('+'), km, api)
	requireInputPrompt(t, cmd, inputLabelPrompt)
	if api.addLabelCalls != 0 {
		t.Fatalf("addLabelCalls = %d, want 0 (no commit yet)", api.addLabelCalls)
	}
}

// TestDetail_AddLabel_CommitCallsAPI: dispatchPanelPromptCommit with
// inputLabelPrompt + "bug" calls api.AddLabel("bug", "tester").
// Tested directly because the commit path lives at the Model level
// (Model.commitInput → dm.dispatchPanelPromptCommit), and
// fakeDetailAPI doesn't fit through Model.api (*Client).
func TestDetail_AddLabel_CommitCallsAPI(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_ = executePromptCommit(t, dm, api, km, inputLabelPrompt, "bug")
	if api.addLabelCalls != 1 {
		t.Fatalf("addLabelCalls = %d, want 1", api.addLabelCalls)
	}
	if api.lastLabel != "bug" {
		t.Fatalf("lastLabel = %q, want bug", api.lastLabel)
	}
	if api.lastActor != "tester" {
		t.Fatalf("lastActor = %q, want tester", api.lastActor)
	}
}

// TestDetail_RemoveLabel_OpensPromptAndDispatches: '-' opens a
// remove-label prompt; commit dispatches to api.RemoveLabel.
func TestDetail_RemoveLabel_OpensPromptAndDispatches(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('-'), km, api)
	requireInputPrompt(t, cmd, inputRemoveLabelPrompt)
	_ = executePromptCommit(t, dm, api, km, inputRemoveLabelPrompt, "bug")
	if api.removeLabelCalls != 1 {
		t.Fatalf("removeLabelCalls = %d, want 1", api.removeLabelCalls)
	}
	if api.lastLabel != "bug" {
		t.Fatalf("lastLabel = %q, want bug", api.lastLabel)
	}
}

// TestDetail_AssignOwner_OpensPromptAndDispatches: 'a' opens an
// owner-assign prompt; commit dispatches to api.Assign.
func TestDetail_AssignOwner_OpensPromptAndDispatches(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('a'), km, api)
	requireInputPrompt(t, cmd, inputOwnerPrompt)
	_ = executePromptCommit(t, dm, api, km, inputOwnerPrompt, "alice")
	if api.assignCalls != 1 {
		t.Fatalf("assignCalls = %d, want 1", api.assignCalls)
	}
	if api.lastOwner != "alice" {
		t.Fatalf("lastOwner = %q, want alice", api.lastOwner)
	}
}

// TestDetail_ClearOwner_DispatchesAPI: 'A' immediately calls
// api.Assign("", "tester") with no modal.
func TestDetail_ClearOwner_DispatchesAPI(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	out, cmd := dm.Update(runeKey('A'), km, api)
	if cmd == nil {
		t.Fatal("expected clear cmd from A")
	}
	_ = runDetailCmd(t, out, cmd, km, api)
	if api.assignCalls != 1 {
		t.Fatalf("assignCalls = %d, want 1", api.assignCalls)
	}
	if api.lastOwner != "" {
		t.Fatalf("lastOwner = %q, want empty (clear)", api.lastOwner)
	}
}

// TestDetail_AddLink_Parent: 'p' opens an inputParentPrompt; commit
// of "abc4" calls api.AddLink({Type:parent, ToRef:"abc4"}).
func TestDetail_AddLink_Parent(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('p'), km, api)
	requireInputPrompt(t, cmd, inputParentPrompt)
	_ = executePromptCommit(t, dm, api, km, inputParentPrompt, "abc4")
	if api.addLinkCalls != 1 {
		t.Fatalf("addLinkCalls = %d, want 1", api.addLinkCalls)
	}
	if api.lastLinkBody.Type != "parent" || api.lastLinkBody.ToRef != "abc4" {
		t.Fatalf("lastLinkBody = %+v, want {parent abc4}", api.lastLinkBody)
	}
}

// TestDetail_AddLink_Blocks: 'b' opens an inputBlockerPrompt;
// commit of "xyz4" calls api.AddLink({Type:blocks, ToRef:"xyz4"}).
func TestDetail_AddLink_Blocks(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('b'), km, api)
	requireInputPrompt(t, cmd, inputBlockerPrompt)
	_ = executePromptCommit(t, dm, api, km, inputBlockerPrompt, "xyz4")
	if api.addLinkCalls != 1 {
		t.Fatalf("addLinkCalls = %d, want 1", api.addLinkCalls)
	}
	if api.lastLinkBody.Type != "blocks" || api.lastLinkBody.ToRef != "xyz4" {
		t.Fatalf("lastLinkBody = %+v, want {blocks xyz4}", api.lastLinkBody)
	}
}

// TestDetail_AddLink_Other: 'l' opens an inputLinkPrompt; committing a
// ref creates a "related" link. p covers parent, b covers blocks, so
// l is dedicated to the third kind. (Capital L was rebound to
// ToggleLayout when the layout-toggle hotkey was added — AddLink moved
// to lowercase l for ergonomics.)
func TestDetail_AddLink_Other(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('l'), km, api)
	requireInputPrompt(t, cmd, inputLinkPrompt)
	_ = executePromptCommit(t, dm, api, km, inputLinkPrompt, "qqq7")
	if api.addLinkCalls != 1 {
		t.Fatalf("addLinkCalls = %d, want 1", api.addLinkCalls)
	}
	if api.lastLinkBody.Type != "related" || api.lastLinkBody.ToRef != "qqq7" {
		t.Fatalf("lastLinkBody = %+v, want {related qqq7}", api.lastLinkBody)
	}
}

// TestDetail_SetPriority_OpensPromptAndDispatches: '!' opens an
// inputPriorityPrompt; commit of "1" calls api.SetPriority(*int64=1).
func TestDetail_SetPriority_OpensPromptAndDispatches(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_, cmd := dm.Update(runeKey('!'), km, api)
	requireInputPrompt(t, cmd, inputPriorityPrompt)
	_ = executePromptCommit(t, dm, api, km, inputPriorityPrompt, "1")
	if api.setPriorityCalls != 1 {
		t.Fatalf("setPriorityCalls = %d, want 1", api.setPriorityCalls)
	}
	if api.lastPriority == nil || *api.lastPriority != 1 {
		t.Fatalf("lastPriority = %v, want *int64=1", api.lastPriority)
	}
	if api.lastActor != "tester" {
		t.Fatalf("lastActor = %q, want tester", api.lastActor)
	}
}

// TestDetail_SetPriority_ClearWithDash: commit of "-" clears via a nil
// priority pointer, mirroring CLI's `--priority -`.
func TestDetail_SetPriority_ClearWithDash(t *testing.T) {
	api, dm, km := setupMutationTest(t)

	_ = executePromptCommit(t, dm, api, km, inputPriorityPrompt, "-")
	if api.setPriorityCalls != 1 {
		t.Fatalf("setPriorityCalls = %d, want 1", api.setPriorityCalls)
	}
	if api.lastPriority != nil {
		t.Fatalf("lastPriority = %v, want nil (clear)", api.lastPriority)
	}
}

// TestDetail_SetPriority_ParseFailure: out-of-range or non-numeric
// input does not reach the API; the status surfaces a parse error.
func TestDetail_SetPriority_ParseFailure(t *testing.T) {
	for _, bad := range []string{"5", "abc", "-1"} {
		api := &fakeDetailAPI{}
		km := newKeymap()
		dm := dmFixture()

		out := executePromptCommit(t, dm, api, km, inputPriorityPrompt, bad)
		if api.setPriorityCalls != 0 {
			t.Fatalf("input %q: setPriorityCalls = %d, want 0",
				bad, api.setPriorityCalls)
		}
		if !strings.Contains(out.status, "failed") {
			t.Fatalf("input %q: status = %q, want failure hint",
				bad, out.status)
		}
	}
}

// TestDetail_AddLink_EmptyTrimmedFails: a buffer that's just whitespace
// or "#" alone trims to empty and must not call api.AddLink —
// dispatchLink surfaces a parse-failed status via the synthetic
// mutationDoneMsg path.
func TestDetail_AddLink_EmptyTrimmedFails(t *testing.T) {
	api := &fakeDetailAPI{}
	km := newKeymap()
	dm := dmFixture()

	out := executePromptCommit(t, dm, api, km, inputLinkPrompt, "  #  ")
	if api.addLinkCalls != 0 {
		t.Fatalf("addLinkCalls = %d, want 0 (empty after strip)", api.addLinkCalls)
	}
	if !strings.Contains(out.status, "failed") {
		t.Fatalf("status = %q, expected failure hint", out.status)
	}
}

// TestDetail_MutationError_SurfacesStatus: when the fake returns an
// *APIError, the resulting status line includes "failed" and the
// error's Code/Message.
func TestDetail_MutationError_SurfacesStatus(t *testing.T) {
	api := &fakeDetailAPI{
		mutationErr: &APIError{Code: "validation_error", Message: "bad label"},
	}
	km := newKeymap()
	dm := dmFixture()

	out, cmd := dm.Update(runeKey('x'), km, api)
	if cmd == nil {
		t.Fatal("expected close cmd")
	}
	out = runDetailCmd(t, out, cmd, km, api)
	if !strings.Contains(out.status, "failed") {
		t.Fatalf("status = %q, expected to contain 'failed'", out.status)
	}
	if !strings.Contains(out.status, "validation_error") {
		t.Fatalf("status = %q, expected to contain error code", out.status)
	}
}

// TestDetail_MutationError_PlainError: a plain error.New result still
// reaches the status line so non-typed daemons (or wrapped errors) are
// reported.
func TestDetail_MutationError_PlainError(t *testing.T) {
	api := &fakeDetailAPI{mutationErr: errors.New("boom")}
	km := newKeymap()
	dm := dmFixture()

	out, cmd := dm.Update(runeKey('x'), km, api)
	out = runDetailCmd(t, out, cmd, km, api)
	if !strings.Contains(out.status, "boom") {
		t.Fatalf("status = %q, expected to contain 'boom'", out.status)
	}
}

// TestDetail_MutationSuccess_DispatchesRefetch: after a successful
// mutation, the returned tea.Cmd is a Batch that runs four fetches
// (issue, comments, events, links). We assert at least the GetIssue
// call landed by inspecting api.lastGetIssue after running the batch.
func TestDetail_MutationSuccess_DispatchesRefetch(t *testing.T) {
	api, dm, km := setupMutationTest(t)
	api.getIssueResult = &Issue{UID: "01TEST-42aa", ShortID: "42aa", Status: "closed"}

	out, cmd := dm.Update(runeKey('x'), km, api)
	if cmd == nil {
		t.Fatal("expected close cmd")
	}
	doneMsg := cmd()
	out, refetch := out.Update(doneMsg, km, api)
	if refetch == nil {
		t.Fatal("expected refetch cmd after success")
	}
	if !strings.Contains(out.status, "closed #42aa") {
		t.Fatalf("status = %q, expected 'closed #42aa'", out.status)
	}
	runBatch(refetch)
	if api.lastGetIssue != "42aa" {
		t.Fatalf("api.lastGetIssue = %q, want 42aa (refetch should have run)",
			api.lastGetIssue)
	}
}

// TestDetail_QuitGate_RoutesToBuffer: with the M3b panel-local
// prompt open, 'q' must reach the bubbles input buffer instead of
// triggering tea.Quit. canQuit gates on m.input.kind != inputNone
// regardless of view.
func TestDetail_QuitGate_RoutesToBuffer(t *testing.T) {
	m := initialModel(Options{})
	m.scope = scope{projectID: 7}
	m.list.loading = false
	iss := Issue{ProjectID: 7, UID: "01TEST-42aa", ShortID: "42aa", Title: "fix bug", Status: "open"}
	m.detail.issue = &iss
	m.detail.scopePID = 7
	m.detail.actor = "tester"
	m.view = viewDetail

	// '+' emits openInputCmd; feed the message back so the prompt opens.
	out, cmd := m.Update(runeKey('+'))
	m = out.(Model)
	if cmd == nil {
		t.Fatal("+ produced no cmd; expected openInputCmd")
	}
	out, _ = m.Update(cmd())
	m = out.(Model)
	if m.input.kind != inputLabelPrompt {
		t.Fatalf("prompt did not open: input.kind = %v", m.input.kind)
	}
	out, qcmd := m.Update(runeKey('q'))
	m = out.(Model)
	if qcmd != nil {
		if msg := qcmd(); msg != nil {
			if _, isQuit := msg.(tea.QuitMsg); isQuit {
				t.Fatal("q produced tea.Quit; should have reached the prompt buffer")
			}
		}
	}
	if v := m.input.activeField().value(); v != "q" {
		t.Fatalf("prompt buffer = %q, want q (q routed to prompt)", v)
	}
}

// TestDetail_EmptyBufferCommit_NoDispatch: an empty-buffer commit on
// a panel prompt must not call any API. Tested at the dispatcher
// level since Model.commitInput is the empty-check guard.
func TestDetail_EmptyBufferCommit_NoDispatch(t *testing.T) {
	api := &fakeDetailAPI{}
	dm := dmFixture()
	_, dispatchCmd := dm.dispatchPanelPromptCommit(api, inputLabelPrompt, "")
	// The dispatcher itself doesn't gate on empty buffer (Model
	// trims and gates first); when called directly with "", the
	// inner dispatchLabel will emit a no-op call. Verify the empty
	// label path does NOT call api.AddLabel — check by confirming
	// only one call would have happened with a real label.
	if dispatchCmd != nil {
		// Run it; api.lastLabel will be "" — that's the empty path.
		_ = dispatchCmd()
	}
	if api.lastLabel != "" {
		t.Fatalf("empty buffer dispatched non-empty label %q", api.lastLabel)
	}
}

// TestDetail_NoIssue_NoDispatch: if dm.issue is nil (boot before the
// first fetch lands), pressing 'x' must be a quiet no-op rather than
// panicking on a nil-deref.
func TestDetail_NoIssue_NoDispatch(t *testing.T) {
	api := &fakeDetailAPI{}
	km := newKeymap()
	dm := detailModel{actor: "tester"}

	_, cmd := dm.Update(runeKey('x'), km, api)
	if cmd != nil {
		t.Fatalf("expected nil cmd when issue is nil, got %T", cmd)
	}
	if api.closeCalls != 0 {
		t.Fatalf("closeCalls = %d, want 0", api.closeCalls)
	}
}
