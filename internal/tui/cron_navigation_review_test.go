package tui

import (
	"encoding/json/jsontext"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestCronTabCancelsPendingInboxNavigation(t *testing.T) {
	inbox := ProjectSummary{ID: 2, Name: "inbox-project"}
	inbox.Metadata.Role = jsontext.Value(`"inbox"`)
	m := newTestModel()
	m.api = &inboxTestAPI{projects: []ProjectSummary{inbox}}
	m.scope = homedScope(7, "example-project")
	h := newCronHost(m)
	_, lookup := h.Update(keyRune('I'))
	if lookup == nil || !h.native.inboxPending {
		t.Fatal("inbox lookup did not start")
	}
	reply := lookup()
	h.Update(keyRune('2'))
	if h.tab != 1 {
		t.Fatal("jobs tab did not open")
	}
	h.Update(reply)
	if h.native.scope.projectID != 7 || h.native.scope.inbox || h.tab != 1 {
		t.Fatal("late Inbox reply changed cron foreground", h.native.scope, h.tab)
	}
}

func TestCronTabWaitsForUndoEvidencePreflight(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	api := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "open", Revision: 5}}
	m := initialModel(Options{})
	m.api = newConnectedUndoClient(t, api)
	m.closeRequiresEvidence = true
	m.undoHistory.push(undoEntry{kind: "reopen", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "example-actor", before: Issue{Status: "closed", ClosedReason: &done, Revision: 4}, after: api.issue, revision: 5})
	h := newCronHost(m)
	_, pending := h.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	if pending == nil || !h.native.undoInFlight {
		t.Fatal("undo preflight did not start")
	}
	h.Update(keyRune('2'))
	h.Update(pending())
	if h.tab != 0 || h.native.input.kind != inputCloseForm {
		t.Fatal("undo evidence form lost foreground", h.tab, h.native.input.kind)
	}
}
