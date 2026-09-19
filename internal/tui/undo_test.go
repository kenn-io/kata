package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

type undoTestAPI struct {
	KataAPI
	issue         Issue
	closeResp     *MutationResp
	closeErr      error
	linkResp      *MutationResp
	closeCalls    int
	writeResp     *MutationResp
	createResp    *MutationResp
	commentResp   *MutationResp
	timedResp     *MutationResp
	instanceUID   string
	authActor     string
	reopenCalls   int
	reopenAfter   *Issue
	reopenErr     error
	links         []LinkEntry
	removedLinkID int64
}

func (f *undoTestAPI) GetInstance(_ context.Context) (InstanceInfo, error) {
	uid := f.instanceUID
	if uid == "" {
		uid = "instance-1"
	}
	return InstanceInfo{InstanceUID: uid, Auth: AuthInfo{Actor: f.authActor}}, nil
}

func (f *undoTestAPI) GetIssueDetail(_ context.Context, _ int64, _ string) (*IssueDetail, error) {
	issue := f.issue
	return &IssueDetail{Issue: &issue}, nil
}

func (f *undoTestAPI) completeResponse(resp *MutationResp) *MutationResp {
	if resp != nil && resp.Issue != nil {
		if resp.Issue.UID == "" {
			resp.Issue.UID = f.issue.UID
		}
		if resp.Issue.ProjectID == 0 {
			resp.Issue.ProjectID = f.issue.ProjectID
		}
	}
	return resp
}

func (f *undoTestAPI) Close(_ context.Context, _ int64, _, _ string) (*MutationResp, error) {
	f.closeCalls++
	return f.completeResponse(f.closeResp), f.closeErr
}

func (f *undoTestAPI) AddLink(_ context.Context, _ int64, _ string, _ LinkBody, _ string) (*MutationResp, error) {
	return f.completeResponse(f.linkResp), nil
}

func (f *undoTestAPI) ListLinks(_ context.Context, _ int64, _ string) ([]LinkEntry, error) {
	return f.links, nil
}

func (f *undoTestAPI) RemoveLink(_ context.Context, _ int64, _ string, linkID int64, _ string) (*MutationResp, error) {
	f.removedLinkID = linkID
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) Reopen(_ context.Context, _ int64, _, _ string) (*MutationResp, error) {
	f.reopenCalls++
	if f.reopenAfter != nil {
		f.issue = *f.reopenAfter
	}
	return f.completeResponse(f.writeResp), f.reopenErr
}

func (f *undoTestAPI) Assign(_ context.Context, _ int64, _, _, _ string) (*MutationResp, error) {
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) ClaimTimedAssignment(_ context.Context, _ int64, _, _ string, _ time.Duration) (*MutationResp, error) {
	return f.completeResponse(f.timedResp), nil
}

func (f *undoTestAPI) SetPriority(_ context.Context, _ int64, _ string, _ *int64, _ string) (*MutationResp, error) {
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) AddLabel(_ context.Context, _ int64, _, _, _ string) (*MutationResp, error) {
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) RemoveLabel(_ context.Context, _ int64, _, _, _ string) (*MutationResp, error) {
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) EditBody(_ context.Context, _ int64, _, _, _ string) (*MutationResp, error) {
	return f.completeResponse(f.writeResp), nil
}

func (f *undoTestAPI) CloseWithEvidence(_ context.Context, _ int64, _ string, _ CloseInput) (*MutationResp, error) {
	return f.completeResponse(f.closeResp), nil
}

func (f *undoTestAPI) CreateIssue(_ context.Context, _ int64, _ CreateIssueBody) (*MutationResp, error) {
	return f.createResp, nil
}

func (f *undoTestAPI) AddComment(_ context.Context, _ int64, _, _, _ string) (*MutationResp, error) {
	return f.commentResp, nil
}

func TestUndoClientRecordsCloseAndBoundsHistory(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, ProjectUID: "01JZ0000000000000000000002", ShortID: "abc4", Status: "open", Revision: 3},
		closeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "closed", Revision: 4}, Changed: true}}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "alice")
	require.NoError(t, err)
	require.Equal(t, 1, f.closeCalls)
	require.NotNil(t, resp.undo)
	require.NotNil(t, resp.undo.entry)
	require.Equal(t, "close", resp.undo.entry.kind)
	require.Equal(t, "01JZ0000000000000000000001", resp.undo.entry.uid)
	require.Equal(t, int64(3), resp.undo.entry.before.Revision)
	require.Equal(t, int64(4), resp.undo.entry.revision)

	var h undoHistory
	for range 21 {
		h.push(*resp.undo.entry)
	}
	require.Len(t, h.entries, 20)
}

func TestUndoClientRejectsSecondWriteUntilCompletion(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeResp: &MutationResp{Issue: &Issue{Status: "closed"}, Changed: true}}
	c := newUndoClient(f)
	first, err := c.Close(context.Background(), 7, "abc4", "alice")
	require.NoError(t, err)
	_, err = c.Close(context.Background(), 7, "abc4", "alice")
	require.ErrorContains(t, err, "in progress")
	require.Equal(t, 1, f.closeCalls)
	first.undo.complete()
	_, err = c.Close(context.Background(), 7, "abc4", "alice")
	require.NoError(t, err)
	require.Equal(t, 2, f.closeCalls)
}

func TestUndoClientCreatesBoundaryForRecurringClose(t *testing.T) {
	recurrenceID := int64(11)
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open", RecurrenceID: &recurrenceID},
		closeResp: &MutationResp{Issue: &Issue{Status: "closed"}, Changed: true}}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "alice")
	require.NoError(t, err)
	require.Nil(t, resp.undo.entry)
	require.Contains(t, resp.undo.boundary, "recurring")
}

func TestUndoClientTimedAssignmentClearsHistory(t *testing.T) {
	f := &undoTestAPI{timedResp: &MutationResp{Changed: true}}
	c := newUndoClient(f)
	resp, err := c.ClaimTimedAssignment(context.Background(), 7, "abc4", "alice", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, resp.undo)
	require.Contains(t, resp.undo.boundary, "timed assignment")
}

func TestUndoClientOwnerEditWithExpiryCreatesBoundary(t *testing.T) {
	expires := time.Now().Add(time.Hour)
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, AssignmentExpiresOn: &expires},
		writeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7}, Changed: true}}
	resp, err := newUndoClient(f).Assign(context.Background(), 7, "abc4", "bob", "alice")
	require.NoError(t, err)
	require.Nil(t, resp.undo.entry)
	require.Contains(t, resp.undo.boundary, "expiry")
}

func TestUndoClientRecordsExactCreatedLink(t *testing.T) {
	link := &LinkEntry{ID: 42, Type: "related", From: LinkPeer{UID: "01JZ0000000000000000000001"}, To: LinkPeer{UID: "01JZ0000000000000000000003"}}
	f := &undoTestAPI{issue: Issue{UID: link.From.UID, ProjectID: 7, Status: "open"},
		linkResp: &MutationResp{Issue: &Issue{UID: link.From.UID, Status: "open"}, Link: link, Changed: true}}
	c := newUndoClient(f)
	resp, err := c.AddLink(context.Background(), 7, "abc4", LinkBody{Type: "related", ToRef: "def4"}, "alice")
	require.NoError(t, err)
	require.Equal(t, int64(42), resp.undo.entry.link.ID)
	require.Equal(t, link.To.UID, resp.undo.entry.link.To.UID)
}

func TestUndoClientRecordsSupportedFieldChanges(t *testing.T) {
	owner := "alice"
	priority := int64(0)
	done := "done"
	for _, tc := range []struct {
		name   string
		before Issue
		after  Issue
		kind   string
		call   func(*undoClient) (*MutationResp, error)
	}{
		{"reopen", Issue{Status: "closed", ClosedReason: &done}, Issue{Status: "open"}, "reopen", func(c *undoClient) (*MutationResp, error) { return c.Reopen(context.Background(), 7, "abc4", "bob") }},
		{"assign", Issue{Status: "open"}, Issue{Status: "open", Owner: &owner}, "owner.assign", func(c *undoClient) (*MutationResp, error) {
			return c.Assign(context.Background(), 7, "abc4", owner, "bob")
		}},
		{"clear owner", Issue{Status: "open", Owner: &owner}, Issue{Status: "open"}, "owner.assign", func(c *undoClient) (*MutationResp, error) {
			return c.Assign(context.Background(), 7, "abc4", "", "bob")
		}},
		{"priority zero", Issue{Status: "open"}, Issue{Status: "open", Priority: &priority}, "priority.set", func(c *undoClient) (*MutationResp, error) {
			return c.SetPriority(context.Background(), 7, "abc4", &priority, "bob")
		}},
		{"label add", Issue{Status: "open"}, Issue{Status: "open"}, "label.add", func(c *undoClient) (*MutationResp, error) {
			return c.AddLabel(context.Background(), 7, "abc4", "urgent", "bob")
		}},
		{"label remove", Issue{Status: "open", Labels: []string{"urgent"}}, Issue{Status: "open"}, "label.remove", func(c *undoClient) (*MutationResp, error) {
			return c.RemoveLabel(context.Background(), 7, "abc4", "urgent", "bob")
		}},
		{"body", Issue{Status: "open", Body: "before"}, Issue{Status: "open", Body: "after"}, "body.edit", func(c *undoClient) (*MutationResp, error) {
			return c.EditBody(context.Background(), 7, "abc4", "after", "bob")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.before.UID = "01JZ0000000000000000000001"
			tc.before.ProjectID = 7
			tc.after.UID = tc.before.UID
			tc.after.ProjectID = 7
			f := &undoTestAPI{issue: tc.before, writeResp: &MutationResp{Issue: &tc.after, Changed: true}}
			resp, err := tc.call(newUndoClient(f))
			require.NoError(t, err)
			require.NotNil(t, resp.undo.entry)
			require.Equal(t, tc.kind, resp.undo.entry.kind)
			require.Equal(t, tc.before.Body, resp.undo.entry.before.Body)
			require.Equal(t, tc.before.Owner, resp.undo.entry.before.Owner)
			require.Equal(t, tc.before.Priority, resp.undo.entry.before.Priority)
		})
	}
}

func TestUndoClientNoopHasNoEntry(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", Status: "open"},
		writeResp: &MutationResp{Changed: false}}
	resp, err := newUndoClient(f).AddLabel(context.Background(), 7, "abc4", "urgent", "bob")
	require.NoError(t, err)
	require.NotNil(t, resp.undo)
	require.Nil(t, resp.undo.entry)
	require.Empty(t, resp.undo.boundary)
}

func TestUndoClientRecordsEvidenceClose(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", Status: "open"},
		closeResp: &MutationResp{Issue: &Issue{Status: "closed", Revision: 2}, Changed: true}}
	resp, err := newUndoClient(f).CloseWithEvidence(context.Background(), 7, "abc4", CloseInput{Actor: "bob", Reason: "done", Message: "finished"})
	require.NoError(t, err)
	require.Equal(t, "close", resp.undo.entry.kind)
	require.Equal(t, "bob", resp.undo.entry.actor)
}

func TestUndoClientUnsafeWritesClearHistory(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		call   func(*undoClient) (*MutationResp, error)
	}{
		{"create", "creation", func(c *undoClient) (*MutationResp, error) {
			return c.CreateIssue(context.Background(), 7, CreateIssueBody{Title: "new", Actor: "bob"})
		}},
		{"comment", "comment", func(c *undoClient) (*MutationResp, error) {
			return c.AddComment(context.Background(), 7, "abc4", "note", "bob")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &undoTestAPI{createResp: &MutationResp{Changed: true}, commentResp: &MutationResp{Changed: true}}
			resp, err := tc.call(newUndoClient(f))
			require.NoError(t, err)
			require.Nil(t, resp.undo.entry)
			require.Contains(t, resp.undo.boundary, tc.reason)
		})
	}
}

func TestUndoClientMissingCreatedLinkIsBoundary(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", Status: "open"},
		linkResp: &MutationResp{Issue: &Issue{Status: "open"}, Changed: true}}
	resp, err := newUndoClient(f).AddLink(context.Background(), 7, "abc4", LinkBody{Type: "parent", ToRef: "def4"}, "bob")
	require.NoError(t, err)
	require.Nil(t, resp.undo.entry)
	require.Contains(t, resp.undo.boundary, "link response")
}

func TestUndoClientWrongIssueMutationResponseIsBoundary(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000009", ProjectID: 7, Status: "closed"}, Changed: true}}
	resp, err := newUndoClient(f).Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err)
	require.Nil(t, resp.undo.entry)
	require.Contains(t, resp.undo.boundary, "different issue")
}

func TestUndoClientUnrelatedCreatedLinkResponseIsBoundary(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		linkResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7}, Changed: true,
			Link: &LinkEntry{ID: 42, Type: "related", From: LinkPeer{UID: "other-1"}, To: LinkPeer{UID: "other-2"}}}}
	resp, err := newUndoClient(f).AddLink(context.Background(), 7, "abc4", LinkBody{Type: "related", ToRef: "def4"}, "bob")
	require.NoError(t, err)
	require.Nil(t, resp.undo.entry)
	require.Contains(t, resp.undo.boundary, "link")
}

func TestModelRecordsCompletedWriteBeforeDetailGenerationGuard(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "closed", Revision: 2}, Changed: true}}
	c := newUndoClient(f)
	m := initialModel(Options{})
	m.api = c
	m.view = viewList
	resp, err := c.Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err)
	updated, _ := m.Update(mutationDoneMsg{origin: "detail", gen: 99, kind: "close", resp: resp})
	m = updated.(Model)
	require.Len(t, m.undoHistory.entries, 1)
	require.Equal(t, "close", m.undoHistory.entries[0].kind)
	_, err = c.Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err, "completion should release the pending write")
}

func TestModelBoundaryClearsPriorUndoActions(t *testing.T) {
	m := initialModel(Options{})
	f := &undoTestAPI{createResp: &MutationResp{Changed: true}}
	c := newUndoClient(f)
	m.api = c
	m.undoHistory.push(undoEntry{kind: "close"})
	resp, err := c.CreateIssue(context.Background(), 7, CreateIssueBody{Title: "new", Actor: "bob"})
	require.NoError(t, err)
	updated, _ := m.Update(mutationDoneMsg{origin: "form", kind: "create", resp: resp})
	m = updated.(Model)
	require.Empty(t, m.undoHistory.entries)
	require.Contains(t, m.undoHistory.boundary, "creation")
}

func TestModelUndoKeyIgnoresActiveInput(t *testing.T) {
	m := initialModel(Options{})
	m.input = newSearchBar(ListFilter{})
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	require.Empty(t, updated.(Model).undoHistory.entries)
}

func TestUndoClientReopensRecordedCloseAfterFreshStateCheck(t *testing.T) {
	done := "done"
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open", Revision: 3},
		closeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4}, Changed: true},
		writeResp: &MutationResp{Issue: &Issue{Status: "open", Revision: 5}, Changed: true}}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err)
	entry := *resp.undo.entry
	require.Equal(t, "instance-1", entry.instanceUID)
	resp.undo.complete()
	f.issue = *f.closeResp.Issue
	outcome := c.undo(context.Background(), entry, false, nil)
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	require.Equal(t, 1, f.reopenCalls)
}

func TestUndoClientRefusesChangedIssueAndChangedDaemon(t *testing.T) {
	done := "done"
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4},
		writeResp: &MutationResp{Issue: &Issue{Status: "open"}, Changed: true}}
	c := newUndoClient(f)
	entry := undoEntry{kind: "close", uid: f.issue.UID, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: f.issue, revision: 4}
	other := "wontfix"
	f.issue.ClosedReason = &other
	f.issue.Revision = 8
	outcome := c.undo(context.Background(), entry, false, nil)
	require.False(t, outcome.changed)
	require.NotEmpty(t, outcome.conflict)
	require.Equal(t, 0, f.reopenCalls)
	outcome.attempt.complete()
	f.issue = entry.after
	f.instanceUID = "instance-2"
	outcome = c.undo(context.Background(), entry, false, nil)
	require.False(t, outcome.changed)
	require.NotEmpty(t, outcome.conflict)
	require.Equal(t, 0, f.reopenCalls)
}

func TestUndoClientRestoresFieldEdits(t *testing.T) {
	owner := "alice"
	priority := int64(0)
	for _, tc := range []struct {
		name    string
		entry   undoEntry
		current Issue
	}{
		{"owner", undoEntry{kind: "owner.assign", before: Issue{}, after: Issue{Owner: &owner}}, Issue{Owner: &owner}},
		{"priority zero", undoEntry{kind: "priority.set", before: Issue{}, after: Issue{Priority: &priority}}, Issue{Priority: &priority}},
		{"body", undoEntry{kind: "body.edit", before: Issue{Body: "before"}, after: Issue{Body: "after"}}, Issue{Body: "after"}},
		{"label add", undoEntry{kind: "label.add", label: "urgent", before: Issue{}, after: Issue{}}, Issue{Labels: []string{"urgent"}}},
		{"label remove", undoEntry{kind: "label.remove", label: "urgent", before: Issue{Labels: []string{"urgent"}}, after: Issue{}}, Issue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid := "01JZ0000000000000000000001"
			tc.current.UID, tc.current.ProjectID, tc.current.Revision = uid, 7, 4
			tc.entry.uid, tc.entry.projectID, tc.entry.revision = uid, 7, 4
			tc.entry.instanceUID, tc.entry.actor = "instance-1", "bob"
			f := &undoTestAPI{issue: tc.current, writeResp: &MutationResp{Issue: &Issue{Revision: 4}, Changed: true}}
			outcome := newUndoClient(f).undo(context.Background(), tc.entry, false, nil)
			require.NoError(t, outcome.err)
			require.True(t, outcome.changed)
		})
	}
}

func TestUndoClientRemovesOnlyExactRecordedLink(t *testing.T) {
	link := LinkEntry{ID: 42, Type: "related", From: LinkPeer{UID: "01JZ0000000000000000000001"}, To: LinkPeer{UID: "01JZ0000000000000000000003"}}
	f := &undoTestAPI{issue: Issue{UID: link.From.UID, ProjectID: 7, Revision: 4}, links: []LinkEntry{link},
		writeResp: &MutationResp{Issue: &Issue{Revision: 4}, Changed: true}}
	entry := undoEntry{kind: "link.add", uid: link.From.UID, projectID: 7, instanceUID: "instance-1", actor: "bob", revision: 4, link: &link}
	outcome := newUndoClient(f).undo(context.Background(), entry, false, nil)
	require.NoError(t, outcome.err)
	require.True(t, outcome.changed)
	require.Equal(t, int64(42), f.removedLinkID)
}

func TestModelUndoKeyReopensLastCloseAndConsumesHistory(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "closed", ClosedReason: &done, Revision: 4},
		writeResp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5}, Changed: true}}
	c := newUndoClient(f)
	m := initialModel(Options{})
	m.api = c
	m.view = viewList
	m.projectsByID[7] = "example-project"
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: f.issue, revision: 4})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	require.NotNil(t, cmd)
	m = updated.(Model)
	result := cmd()
	updated, _ = m.Update(result)
	m = updated.(Model)
	require.Empty(t, m.undoHistory.entries)
	require.Equal(t, 1, f.reopenCalls)
	require.Contains(t, m.toast.text, "example-project#abc4")
}

func TestModelUndoKeyKeepsConflictingEntry(t *testing.T) {
	done, other := "done", "wontfix"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "closed", ClosedReason: &other, Revision: 4}}
	m := initialModel(Options{})
	m.api = newUndoClient(f)
	m.view = viewDetail
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: Issue{Status: "closed", ClosedReason: &done}, revision: 4})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	require.NotNil(t, cmd)
	m = updated.(Model)
	updated, _ = m.Update(cmd())
	m = updated.(Model)
	require.Len(t, m.undoHistory.entries, 1)
	require.Equal(t, 0, f.reopenCalls)
}

func TestModelUndoReopenOpensDoneEvidenceForm(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "open", Revision: 5}}
	m := initialModel(Options{})
	m.api = newUndoClient(f)
	m.view = viewList
	m.closeRequiresEvidence = true
	m.undoHistory.push(undoEntry{kind: "reopen", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "closed", ClosedReason: &done, Revision: 4}, after: f.issue, revision: 5})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	require.NotNil(t, cmd)
	updated, _ = updated.(Model).Update(cmd())
	m = updated.(Model)
	require.Equal(t, inputCloseForm, m.input.kind)
	require.Equal(t, "done", m.input.fieldValue(fieldCloseReason))
	require.Equal(t, uid, m.input.target.issueShortID)
	require.Len(t, m.undoHistory.entries, 1)
}

func TestModelUndoReopenSubmitsExistingEvidenceForm(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "open", Revision: 5},
		closeResp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 6}, Changed: true}}
	m := initialModel(Options{})
	m.api = newUndoClient(f)
	m.view = viewList
	m.closeRequiresEvidence = true
	m.undoHistory.push(undoEntry{kind: "reopen", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "closed", ClosedReason: &done, Revision: 4}, after: f.issue, revision: 5})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	updated, _ = updated.(Model).Update(cmd())
	m = updated.(Model)
	m.input.field(fieldCloseMessage).area.SetValue("Completed after review")
	m.input.field(fieldEvidenceValue).input.SetValue("abc123")
	m, cmd = m.commitFormInput(inputCloseForm)
	require.NotNil(t, cmd)
	updated, _ = m.Update(cmd())
	m = updated.(Model)
	require.Equal(t, inputNone, m.input.kind)
	require.Empty(t, m.undoHistory.entries)
}

func TestModelCancelUndoEvidenceFormKeepsEntry(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{kind: "reopen"})
	m = m.openUndoCloseForm()
	m, _ = m.cancelInput()
	require.Equal(t, inputNone, m.input.kind)
	require.Zero(t, m.undoCloseEntryID)
	require.Len(t, m.undoHistory.entries, 1)
}

func TestModelFooterOffersUndoOnlyWhenHistoryExists(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewList
	m.width, m.height = 100, 35
	m.list.loading = false
	m.list.issues = []Issue{{UID: "01JZ0000000000000000000001", ShortID: "abc4", Title: "Example", Status: "open"}}
	without := m.View().Content
	require.NotContains(t, stripANSI(without), "u undo")
	m.undoHistory.push(undoEntry{kind: "close"})
	with := m.View().Content
	require.Contains(t, stripANSI(with), "u undo")
}

func TestUndoClientDefinitiveRefusalKeepsHistory(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeErr: &APIError{Status: 403, Code: "forbidden", Message: "cannot close"}}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "bob")
	require.Error(t, err)
	require.NotNil(t, resp.undo)
	require.False(t, resp.undo.unknown)
	m := initialModel(Options{})
	m.api = c
	m.undoHistory.push(undoEntry{kind: "owner.assign"})
	updated, _ := m.Update(mutationDoneMsg{origin: "detail", kind: "close", resp: resp, err: err})
	require.Len(t, updated.(Model).undoHistory.entries, 1)
}

func TestUndoClientTransportFailureClearsHistory(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeErr: context.DeadlineExceeded}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "bob")
	require.Error(t, err)
	require.True(t, resp.undo.unknown)
	m := initialModel(Options{})
	m.api = c
	m.undoHistory.push(undoEntry{kind: "owner.assign"})
	updated, _ := m.Update(mutationDoneMsg{origin: "detail", kind: "close", resp: resp, err: err})
	require.Empty(t, updated.(Model).undoHistory.entries)
}

func TestUndoClientRefusesChangedPrincipal(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{authActor: "alice", issue: Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 3},
		closeResp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4}, Changed: true}}
	c := newUndoClient(f)
	resp, err := c.Close(context.Background(), 7, "abc4", "alice")
	require.NoError(t, err)
	entry := *resp.undo.entry
	resp.undo.complete()
	f.issue = *f.closeResp.Issue
	f.authActor = "mallory"
	out := c.undo(context.Background(), entry, false, nil)
	require.Contains(t, out.conflict, "principal")
	require.Equal(t, 0, f.reopenCalls)
}

func TestUndoClientAlreadyRestoredConsumesEntryWithoutWrite(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5}}
	m := initialModel(Options{})
	m.api = newUndoClient(f)
	m.view = viewList
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: Issue{Status: "closed", ClosedReason: &done}, revision: 4})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	updated, _ = updated.(Model).Update(cmd())
	require.Empty(t, updated.(Model).undoHistory.entries)
	require.Equal(t, 0, f.reopenCalls)
}

func TestUndoHistoryDoesNotRebaseUnexpectedRevision(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{uid: "issue-a", projectID: 7, instanceUID: "instance-1", revision: 4})
	m.rebaseUndoRevisions(undoEntry{uid: "issue-a", projectID: 7, instanceUID: "instance-1", kind: "close", before: Issue{Revision: 4}},
		&MutationResp{Issue: &Issue{Revision: 99}})
	require.Equal(t, int64(4), m.undoHistory.entries[0].revision)
}

func TestUndoClientNoopInverseReadbackConsumesRestoredEntry(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4},
		reopenAfter: &Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5},
		writeResp:   &MutationResp{Changed: false}}
	entry := undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: f.issue, revision: 4}
	out := newUndoClient(f).undo(context.Background(), entry, false, nil)
	require.NoError(t, out.err)
	require.True(t, out.already)
	require.False(t, out.changed)
	require.Equal(t, 1, f.reopenCalls)
}

func TestModelChangedPrincipalClearsUndoHistory(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{kind: "close"})
	entryID := m.undoHistory.entries[0].id
	out := undoOutcome{conflict: "daemon principal changed"}
	updated, _ := m.Update(undoDoneMsg{entryID: entryID, outcome: out})
	require.Empty(t, updated.(Model).undoHistory.entries)
}

func TestModelChangedPrincipalClosesUndoEvidenceForm(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{kind: "reopen"})
	m = m.openUndoCloseForm()
	entryID, formGen := m.undoCloseEntryID, m.input.formGen
	updated, _ := m.Update(undoDoneMsg{entryID: entryID, formGen: formGen,
		outcome: undoOutcome{conflict: "daemon principal changed"}})
	m = updated.(Model)
	require.Empty(t, m.undoHistory.entries)
	require.Equal(t, inputNone, m.input.kind)
	require.Zero(t, m.undoCloseEntryID)
}

func TestModelCapabilityRefreshClearsHistoryForChangedPrincipal(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{kind: "close", auth: AuthInfo{Actor: "alice"}})
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "bob"}})
	require.Empty(t, m.undoHistory.entries)
	require.Contains(t, m.undoHistory.boundary, "principal")
}

func TestModelDaemonSwitchClearsHistoryAndLateCompletion(t *testing.T) {
	f := &undoTestAPI{issue: Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "open"},
		closeResp: &MutationResp{Issue: &Issue{UID: "01JZ0000000000000000000001", ProjectID: 7, Status: "closed"}, Changed: true}}
	oldClient := newUndoClient(f)
	m := initialModel(Options{})
	m.api = oldClient
	m.connGen = 1
	m.undoHistory.push(undoEntry{kind: "owner.assign"})
	m.undoInFlight = true
	resp, err := oldClient.Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err)
	m, _ = m.installDaemonConnection(daemonConnection{api: &Client{}, init: bootInit{view: viewEmpty}})
	require.Empty(t, m.undoHistory.entries)
	require.False(t, m.undoInFlight)
	updated, _ := m.Update(mutationDoneMsg{connGen: 1, origin: "detail", kind: "close", resp: resp})
	m = updated.(Model)
	require.Empty(t, m.undoHistory.entries)
	_, err = oldClient.Close(context.Background(), 7, "abc4", "bob")
	require.NoError(t, err, "late old completion must release the old client")
}

func TestModelDropsListFetchDispatchedBeforeUndo(t *testing.T) {
	m := initialModel(Options{})
	m.view = viewList
	m.scope = scope{projectID: 7}
	m.list.loading = false
	m.list.issues = []Issue{{UID: "issue-a", Status: "open", Title: "corrected"}}
	m.mutationEpoch = 1
	old := initialFetchMsg{dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit}, epochSet: true, epoch: 0,
		issues: []Issue{{UID: "issue-a", Status: "closed", Title: "stale"}}}
	updated, _ := m.Update(old)
	require.Equal(t, "corrected", updated.(Model).list.issues[0].Title)
}

func TestModelUndoRefreshesVisibleDetailImmediately(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	m := initialModel(Options{})
	m.view = viewDetail
	m.detail.issue = &Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4}
	m.detail.scopePID = 7
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, revision: 4,
		before: Issue{Status: "open", Revision: 3}, after: *m.detail.issue})
	entryID := m.undoHistory.entries[0].id
	updated, _ := m.Update(undoDoneMsg{entryID: entryID,
		outcome: undoOutcome{changed: true, resp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5}, Changed: true}}})
	m = updated.(Model)
	require.Equal(t, "open", m.detail.issue.Status)
	require.Empty(t, m.undoHistory.entries)
}

func TestUndoHistoryRebasesInterleavedSameIssueEntries(t *testing.T) {
	m := initialModel(Options{})
	m.undoHistory.push(undoEntry{uid: "issue-a", projectID: 7, instanceUID: "instance-1", revision: 4})
	m.undoHistory.push(undoEntry{uid: "issue-b", projectID: 7, instanceUID: "instance-1", revision: 2})
	m.rebaseUndoRevisions(undoEntry{uid: "issue-a", projectID: 7, instanceUID: "instance-1", kind: "close",
		before: Issue{Revision: 4}, revision: 5}, &MutationResp{Issue: &Issue{Revision: 6}})
	require.Equal(t, int64(6), m.undoHistory.entries[0].revision)
	require.Equal(t, int64(2), m.undoHistory.entries[1].revision)
}

func TestModelUndoDefinitiveRefusalKeepsEntry(t *testing.T) {
	done := "done"
	uid := "01JZ0000000000000000000001"
	f := &undoTestAPI{issue: Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done, Revision: 4},
		reopenErr: &APIError{Status: 403, Code: "forbidden", Message: "cannot reopen"}}
	m := initialModel(Options{})
	m.api = newUndoClient(f)
	m.view = viewList
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, instanceUID: "instance-1", actor: "bob",
		before: Issue{Status: "open", Revision: 3}, after: f.issue, revision: 4})
	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'u', Text: "u"})
	updated, _ = updated.(Model).Update(cmd())
	require.Len(t, updated.(Model).undoHistory.entries, 1)
}

func TestModelUndoRejectsOldDetailResultAfterCorrection(t *testing.T) {
	// Represent a detail request dispatched before the correction.
	oldRequestSeq := nextDetailFetchRequestSeq.Add(1)
	done := "done"
	uid := "01JZ0000000000000000000001"
	m := initialModel(Options{})
	m.view = viewDetail
	m.detail.gen = 8
	m.detail.scopePID = 7
	m.detail.issue = &Issue{UID: uid, ProjectID: 7, ShortID: "abc4", Status: "closed", ClosedReason: &done, Revision: 4}
	m.undoHistory.push(undoEntry{kind: "close", uid: uid, projectID: 7, revision: 4,
		before: Issue{Status: "open", Revision: 3}, after: *m.detail.issue})
	updated, _ := m.Update(undoDoneMsg{entryID: m.undoHistory.entries[0].id,
		outcome: undoOutcome{changed: true, resp: &MutationResp{Issue: &Issue{UID: uid, ProjectID: 7, Status: "open", Revision: 5}, Changed: true}}})
	m = updated.(Model)
	old := detailFetchedMsg{gen: 8, requestSeq: oldRequestSeq,
		issue: &Issue{UID: uid, ProjectID: 7, Status: "closed", ClosedReason: &done}}
	updated, _ = m.Update(old)
	require.Equal(t, "open", updated.(Model).detail.issue.Status)
}
