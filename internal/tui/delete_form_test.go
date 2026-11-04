package tui

import (
	"encoding/json/jsontext"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/testenv"
	"go.kenn.io/kit/tui/helplayout"
	"go.kenn.io/kit/tui/splitlayout"
)

// deleteHarness drives the Delete form over three issues held by a real daemon.
type deleteHarness struct {
	formDriver
	t       *testing.T
	env     *testenv.Env
	client  *undoClient
	project db.Project
	issues  []db.Issue
}

func newDeleteHarness(t *testing.T, view viewID) *deleteHarness {
	t.Helper()
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	h := &deleteHarness{t: t, env: env, project: project,
		client: newConnectedUndoClient(t, NewClient(env.URL, env.HTTP))}
	var rows []Issue
	for _, title := range []string{"First", "Second", "Third"} {
		issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: title, Author: "alice",
		})
		require.NoError(t, err)
		h.issues = append(h.issues, issue)
		detail, err := h.client.GetIssueDetail(ctx, project.ID, issue.UID)
		require.NoError(t, err)
		rows = append(rows, *detail.Issue)
	}
	h.m = initialModel(Options{})
	h.m.api = h.client
	h.m.list.actor = "alice"
	h.m.scope = scope{projectID: project.ID, projectName: project.Name}
	h.m.list.issues = rows
	h.m.view = view
	return h
}

// selectIssue puts the cursor on the issue with the given uid, and in the
// detail view opens that issue there.
func (h *deleteHarness) selectIssue(uid string) {
	h.t.Helper()
	rows := h.m.list.visibleRows()
	for i, row := range rows {
		if row.issue.UID == uid {
			h.m.list = h.m.list.selectVisibleRow(rows, i)
			if h.m.view == viewDetail {
				issue := row.issue
				h.m.detail = detailModel{scopePID: h.project.ID, issue: &issue}
			}
			return
		}
	}
	h.t.Fatalf("issue %s is not listed", uid)
}

func (h *deleteHarness) enter() tea.Cmd { return h.key(tea.KeyPressMsg{Code: tea.KeyEnter}) }

// finish feeds the daemon's answer to a command back into the model.
func (h *deleteHarness) finish(cmd tea.Cmd) {
	h.t.Helper()
	require.NotNil(h.t, cmd)
	out, _ := h.m.Update(cmd())
	h.m = out.(Model)
}

// drain follows fetch results and their Back commands until navigation settles.
func (h *deleteHarness) drain(cmd tea.Cmd) Model {
	h.t.Helper()
	if cmd == nil {
		return h.m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, work := range batch {
			h.m = h.drain(work)
		}
		return h.m
	}
	next, follow := h.m.Update(msg)
	h.m = next.(Model)
	switch msg.(type) {
	case detailFetchedMsg, commentsFetchedMsg, eventsFetchedMsg, linksFetchedMsg, openDetailMsg, jumpDetailMsg, popDetailMsg, mutationDoneMsg, undoDoneMsg:
		return h.drain(follow)
	}
	return h.m
}

func (h *deleteHarness) isDeleted(uid string) bool {
	h.t.Helper()
	_, err := h.env.DB.IssueByUID(h.t.Context(), uid, db.IncludeDeletedNo)
	if err == nil {
		return false
	}
	require.ErrorIs(h.t, err, db.ErrNotFound)
	return true
}

func (h *deleteHarness) qualified(issue db.Issue) string {
	return h.project.Name + "#" + issue.ShortID
}

func TestDeleteFormShowsConfirmationOnceAtEveryHeight(t *testing.T) {
	for _, height := range []int{10, 11, 30} {
		form := newDeleteForm(formTarget{}, Issue{Title: "Duplicate issue"}, "example-project#abc4")
		rendered := stripANSI(renderCenteredForm(form, 100, height, snapshotFixedNow))
		assert.Equal(t, 1, strings.Count(rendered, "DELETE example-project#abc4"), "height %d", height)
	}
}

func TestDeleteFormShowsFullConfirmationAt80Columns(t *testing.T) {
	form := newDeleteForm(formTarget{}, Issue{Title: "Duplicate issue"}, "example-repository-project-task-tracker#abc4")
	rendered := stripANSI(renderCenteredForm(form, 80, 24, snapshotFixedNow))
	assert.Contains(t, rendered, "DELETE example-repository-project-task-tracker#abc4")
}

func TestDeleteFormRequiresFullConfirmation(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)

	h.key(runeKey('X'))
	require.Equal(t, inputDeleteForm, h.m.input.kind, "X opens the Delete form")
	rendered := stripANSI(renderCenteredForm(h.m.input, 100, 30, h.m.toastNow()))
	assert.Contains(t, rendered, "DELETE "+h.qualified(target))
	assert.Empty(t, h.m.input.field(fieldConfirm).input.Placeholder)
	assert.Contains(t, rendered, "Second", "the form shows the issue title")

	for _, typed := range []string{"", h.issues[1].ShortID, h.qualified(target), h.qualified(target)[1:], "DELETE " + h.qualified(h.issues[0]), "delete " + h.qualified(target), " DELETE " + h.qualified(target), "DELETE " + h.qualified(target) + " "} {
		h.m.input.field(fieldConfirm).setValue(typed)
		cmd := h.enter()
		assert.Nil(t, cmd, "enter does nothing for %q", typed)
		assert.Equal(t, inputDeleteForm, h.m.input.kind)
		assert.False(t, h.isDeleted(target.UID), "nothing is deleted for %q", typed)
	}

	h.m.input.field(fieldConfirm).setValue("")
	h.m = typeString(h.m, "DELETE "+h.qualified(target))
	h.finish(h.enter())
	assert.Equal(t, inputNone, h.m.input.kind, "a delete closes the form: "+h.m.input.err)
	assert.True(t, h.isDeleted(target.UID))
}

func TestDeleteSelectsTheNeighbouringRowAndNamesUndo(t *testing.T) {
	cases := []struct {
		name              string
		view              viewID
		target, neighbour int
	}{
		{"list", viewList, 1, 2},
		{"detail", viewDetail, 1, 2},
		{"last row selects the previous row", viewList, 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, tc.view)
			rows := h.m.list.visibleRows()
			require.Len(t, rows, 3)
			target, neighbour := rows[tc.target].issue, rows[tc.neighbour].issue
			h.selectIssue(target.UID)

			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.project.Name+"#"+target.ShortID)
			h.finish(h.enter())

			assert.True(t, h.isDeleted(target.UID))
			selected, ok := h.m.list.targetRow()
			require.True(t, ok)
			assert.Equal(t, neighbour.UID, selected.UID, "selection moves to the neighbouring row")
			assert.Equal(t, viewList, h.m.view, "a deleted issue's detail is left")
			require.NotNil(t, h.m.toast)
			assert.Contains(t, h.m.toast.text, "press u to undo", "the notice names the undo key")
			assert.Contains(t, h.m.toast.text, h.project.Name+"#"+target.ShortID)
		})
	}
}

func TestUndoOfADelete(t *testing.T) {
	cases := []struct {
		name              string
		restoredElsewhere bool
		wantToast         string
	}{
		{"restores the issue", false, "undid delete"},
		{"reports an issue that is no longer deleted", true, "already restored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			target := h.issues[0]
			h.selectIssue(target.UID)
			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))
			require.Len(t, h.m.undoHistory.entries, 1)
			assert.Equal(t, h.qualified(target), h.m.undoHistory.entries[0].before.QualifiedID)
			if tc.restoredElsewhere {
				_, _, changed, err := h.env.DB.RestoreIssue(t.Context(), target.ID, "bob")
				require.NoError(t, err)
				require.True(t, changed)
			}

			h.finish(h.key(runeKey('u')))

			assert.False(t, h.isDeleted(target.UID))
			assert.Empty(t, h.m.undoHistory.entries, "the spent entry is dropped")
			require.NotNil(t, h.m.toast)
			assert.Equal(t, toastInfo, h.m.toast.level, "an already restored issue is not an error")
			if tc.restoredElsewhere {
				assert.Contains(t, h.m.toast.text, tc.wantToast)
			} else {
				assert.Equal(t, "undid delete on "+h.qualified(target), h.m.toast.text)
			}
		})
	}
}

func TestUndoDeleteAfterComment(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edits int
	}{
		{"comment", 0},
		{"history limit then comment", undoLimit + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			target, survivor := h.issues[0], h.issues[1]
			h.selectIssue(target.UID)
			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))
			for i := range tc.edits {
				resp, err := h.client.EditBody(t.Context(), h.project.ID, survivor.UID, strings.Repeat("x", i+1), "alice")
				require.NoError(t, err)
				next, _ := h.m.Update(mutationDoneMsg{resp: resp})
				h.m = next.(Model)
			}
			resp, err := h.client.AddComment(t.Context(), h.project.ID, survivor.UID, "Removed the duplicate", "alice")
			require.NoError(t, err)
			next, _ := h.m.Update(mutationDoneMsg{resp: resp})
			h.m = next.(Model)

			cmd := h.key(runeKey('u'))
			if tc.edits > undoLimit {
				require.True(t, h.isDeleted(target.UID))
				assert.Contains(t, h.m.toast.text, "nothing to undo")
			} else {
				h.finish(cmd)
				require.False(t, h.isDeleted(target.UID))
				assert.Contains(t, h.m.toast.text, "undid delete")
			}
		})
	}
}

func TestDeleteKeyIsOfferedOnlyToCredentialsThatCanDelete(t *testing.T) {
	hint := helplayout.HelpItem{Key: "X", Description: "delete"}
	for _, scoped := range []bool{false, true} {
		for _, view := range []viewID{viewList, viewDetail} {
			h := newDeleteHarness(t, view)
			h.selectIssue(h.issues[0].UID)
			if view == viewDetail {
				h.m.focus = focusDetail
			}
			h.m.authCapabilitiesRequired, h.m.authCapabilitiesReady = true, true
			h.m.issueScoped, h.m.scopedWritable = scoped, scoped

			cmd := h.key(runeKey('X'))
			helpText := stripANSI(renderHelp(h.m.helpKeymap(), 120, ListFilter{}))
			if scoped {
				assert.Nil(t, cmd)
				assert.Equal(t, inputNone, h.m.input.kind, "X does nothing under scoped credentials")
				assert.False(t, h.isDeleted(h.issues[0].UID))
				assert.NotContains(t, helpText, "delete issue")
			} else {
				assert.Equal(t, inputDeleteForm, h.m.input.kind)
			}
			h.m.input = inputState{}
			for _, rows := range [][][]helplayout.HelpItem{h.m.helpRows(), h.m.splitHelpRows()} {
				if scoped {
					assertHelpItemAbsent(t, flattenHelpRows(rows), hint)
				}
			}
		}
	}
}

// A deleted issue's detail is left the way back leaves it, and the list
// selection only moves when the deleted issue was the selected row.
func TestDeleteLeavesTheDetailTheWayBackDoes(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *deleteHarness)
		check func(t *testing.T, h *deleteHarness)
	}{
		{
			name: "stacked detail reached by a jump",
			setup: func(h *deleteHarness) {
				h.m.layout = splitlayout.Stacked
			},
			check: func(t *testing.T, h *deleteHarness) {
				assert.Equal(t, viewDetail, h.m.view)
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, h.issues[0].UID, h.m.detail.issue.UID)
				assert.Empty(t, h.m.detail.navStack)
			},
		},
		{
			name: "split detail returns focus to list",
			setup: func(h *deleteHarness) {
				h.m.view = viewList
				h.m.layout, h.m.focus = splitlayout.Split, focusDetail
				h.m.detail.navStack = nil
				h.selectIssue(h.m.detail.issue.UID)
			},
			check: func(t *testing.T, h *deleteHarness) {
				assert.Equal(t, focusList, h.m.focus)
				assert.Equal(t, viewList, h.m.view)
				require.NotNil(t, h.m.detail.issue)
				selected, ok := h.m.list.targetRow()
				require.True(t, ok)
				assert.Equal(t, selected.UID, h.m.detail.issue.UID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			rows := h.m.list.visibleRows()
			selected, target := rows[0].issue, rows[2].issue
			h.selectIssue(selected.UID)
			h.m.detail.gen = 1
			parent := cloneDetailModel(h.m.detail)
			h.m.detail = detailModel{scopePID: h.project.ID, issue: &target, gen: 2,
				navStack: []detailModel{parent}}
			h.m.nextGen = 2
			tc.setup(h)

			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.project.Name+"#"+target.ShortID)
			h.m = h.drain(h.enter())
			require.True(t, h.isDeleted(target.UID))

			current, ok := h.m.list.targetRow()
			require.True(t, ok)
			if h.m.layout == splitlayout.Stacked {
				assert.Equal(t, selected.UID, current.UID, "the selection stays on the row that was selected")
			}
			tc.check(t, h)
		})
	}
}

func TestDeleteSavingEscKeepsResult(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)
	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(target))
	cmd := h.enter()
	require.NotNil(t, cmd)
	h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, inputDeleteForm, h.m.input.kind)
	h.finish(cmd)
	assert.True(t, h.isDeleted(target.UID))
}

func TestDeleteDetailStaysGoneAfterWidening(t *testing.T) {
	h := newDeleteHarness(t, viewDetail)
	h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Equal(t, splitlayout.Stacked, h.m.layout)
	target := h.issues[0]
	h.selectIssue(target.UID)
	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(target))
	h.finish(h.enter())
	require.True(t, h.isDeleted(target.UID))
	require.Equal(t, viewList, h.m.view)
	assert.Nil(t, h.m.detail.issue, "returning to the stacked list clears the deleted detail")

	var cmd tea.Cmd
	h.m, cmd = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
	h.m = drainInboxCmds(h.m, cmd)
	require.Equal(t, splitlayout.Split, h.m.layout)
	require.NotNil(t, h.m.detail.issue)
	selected, ok := h.m.list.targetRow()
	require.True(t, ok)
	assert.Equal(t, selected.UID, h.m.detail.issue.UID)
	assert.NotEqual(t, target.UID, h.m.detail.issue.UID)
}

func TestDeleteSkipsRepeatedIssueInDetailHistory(t *testing.T) {
	for _, source := range []string{"detail", "Attention"} {
		t.Run(source, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			h.m.layout = splitlayout.Stacked
			target, other := h.issues[0], h.issues[1]
			h.selectIssue(target.UID)
			var history []detailModel
			for i, issue := range []db.Issue{target, other, target} {
				h.selectIssue(issue.UID)
				h.m.detail.gen = int64(i + 1)
				history = append(history, cloneDetailModel(h.m.detail))
			}
			h.m.detail.navStack = history
			h.m.detail.gen, h.m.nextGen = 4, 4
			if source == "Attention" {
				t.Setenv("KATA_INBOX_USER", "alice")
				_, err := h.env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
					IssueID: target.ID, Actor: "alice", IfMatchRev: new(target.Revision),
					Patch: map[string]jsontext.Value{notification.MetadataKey("alice"): jsontext.Value(`{"from":"bob","message":"Review this"}`)},
				})
				require.NoError(t, err)
				var cmd tea.Cmd
				h.m, cmd = h.m.transitionToAttention()
				h.m = h.drain(cmd)
				next, cmd := h.m.Update(h.enter()())
				h.m = next.(Model)
				h.m = h.drain(cmd)
			}
			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			next, cmd := h.m.Update(h.enter()())
			h.m = next.(Model)
			h.m = h.drain(cmd)
			require.True(t, h.isDeleted(target.UID))
			if source == "Attention" {
				require.Equal(t, viewAttention, h.m.view)
				cmd = h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
				h.m = h.drain(cmd)
			}
			require.Equal(t, viewDetail, h.m.view)
			require.NotNil(t, h.m.detail.issue)
			assert.Equal(t, other.UID, h.m.detail.issue.UID, "delete skips consecutive visits to the deleted issue")
			cmd = h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
			h.m = h.drain(cmd)
			assert.Equal(t, viewList, h.m.view, "Back skips the earlier visit to the deleted issue")
			assert.True(t, h.m.detail.issue == nil || h.m.detail.issue.UID != target.UID)
		})
	}
}

func TestDeleteChildRefetchesParent(t *testing.T) {
	h := newDeleteHarness(t, viewDetail)
	parent, child := h.issues[0], h.issues[1]
	base := NewClient(h.env.URL, h.env.HTTP)
	_, err := base.AddLink(t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(t, err)
	_, err = base.AddLabel(t.Context(), h.project.ID, child.UID, "bug", "alice")
	require.NoError(t, err)
	var cmd tea.Cmd
	h.m, cmd = h.m.dispatchLabelFetch(h.project.ID)
	h.m = h.drain(cmd)
	require.Equal(t, []LabelCount{{Label: "bug", Count: 1}}, h.m.projectLabels.byProject[h.project.ID].labels)
	h.selectIssue(parent.UID)
	h.m.detail.gen = 1
	h.m.nextGen = 1
	cmd = h.m.detail.refetch(h.m.api)
	h.m = h.drain(cmd)
	require.Len(t, h.m.detail.children, 1)
	h.m.detail.detailFocus = focusChildren
	h.m = h.drain(h.enter())
	require.Equal(t, child.UID, h.m.detail.issue.UID)
	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(child))
	h.m = h.drain(h.enter())
	require.Equal(t, parent.UID, h.m.detail.issue.UID)
	assert.Empty(t, h.m.detail.children, "the restored parent drops its deleted child")
	assert.Empty(t, h.m.projectLabels.byProject[h.project.ID].labels)
	h.m = h.drain(h.key(runeKey('u')))
	require.False(t, h.isDeleted(child.UID))
	require.Equal(t, parent.UID, h.m.detail.issue.UID)
	require.Len(t, h.m.detail.children, 1, "undo shows the restored child in the open parent")
	assert.Equal(t, child.UID, h.m.detail.children[0].UID)
	assert.Equal(t, []LabelCount{{Label: "bug", Count: 1}}, h.m.projectLabels.byProject[h.project.ID].labels)
}

func TestBackLeavesDeletedSavedDetail(t *testing.T) {
	for _, source := range []string{"history", "Attention", "Inbox"} {
		t.Run(source, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			h.m.layout = splitlayout.Stacked
			target, other := h.issues[0], h.issues[1]
			h.selectIssue(target.UID)
			h.m.detail.gen, h.m.nextGen = 1, 1
			var cmd tea.Cmd
			switch source {
			case "history":
				prior := cloneDetailModel(h.m.detail)
				h.selectIssue(other.UID)
				h.m.detail.gen, h.m.nextGen = 2, 2
				h.m.detail.navStack = []detailModel{prior}
			case "Attention":
				t.Setenv("KATA_INBOX_USER", "alice")
				h.m, cmd = h.m.transitionToAttention()
				h.m = h.drain(cmd)
			case "Inbox":
				inbox, err := h.env.DB.CreateProject(t.Context(), "inbox-project")
				require.NoError(t, err)
				h.m.inboxPending = true
				h.m, cmd = h.m.handleInboxProjectLoaded(inboxProjectLoadedMsg{
					project: ProjectSummary{ID: inbox.ID, Name: inbox.Name}, found: true,
				})
				h.m = h.drain(cmd)
			}
			_, err := NewClient(h.env.URL, h.env.HTTP).DeleteIssue(t.Context(), h.project.ID, target.UID, h.qualified(target), "alice")
			require.NoError(t, err)
			require.True(t, h.isDeleted(target.UID))

			cmd = h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
			h.m = h.drain(cmd)

			assert.Equal(t, viewList, h.m.view, "Back leaves a saved issue that no longer exists")
			assert.True(t, h.m.detail.issue == nil || h.m.detail.issue.UID != target.UID, "the deleted snapshot is cleared")
		})
	}
}

func TestDeleteReturnsToAttentionWithoutUndoHint(t *testing.T) {
	t.Setenv("KATA_INBOX_USER", "alice")
	h := newDeleteHarness(t, viewDetail)
	h.m.layout = splitlayout.Stacked
	target := h.issues[0]
	h.selectIssue(target.UID)
	_, err := h.env.DB.PatchIssueMetadata(t.Context(), db.PatchIssueMetadataIn{
		IssueID: target.ID, Actor: "alice", IfMatchRev: new(target.Revision),
		Patch: map[string]jsontext.Value{notification.MetadataKey("alice"): jsontext.Value(`{"from":"bob","message":"Review this"}`)},
	})
	require.NoError(t, err)
	var cmd tea.Cmd
	h.m, cmd = h.m.transitionToAttention()
	h.m = h.drain(cmd)
	h.m = h.drain(h.enter())
	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(target))
	h.finish(h.enter())
	require.Equal(t, viewAttention, h.m.view)
	require.NotNil(t, h.m.toast)
	assert.Equal(t, "deleted "+h.qualified(target), h.m.toast.text)
	assert.Nil(t, h.key(runeKey('u')), "undo is offered in list and detail")
	assert.True(t, h.isDeleted(target.UID))
}

func TestDetailFetchErrorsOnlyLeaveMissingCurrentIssue(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  int64
		seq  uint64
		code string
	}{
		{"old navigation", 1, 0, "issue_not_found"},
		{"old request", 2, 4, "issue_not_found"},
		{"other error", 2, 6, "forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := initialModel(Options{})
			m.view = viewDetail
			m.layout = splitlayout.Stacked
			m.detail.issue = &Issue{UID: "current-issue"}
			m.detail.gen = 2
			m.detail.fetchSeq.issue = 5
			m, _ = updateModel(m, detailFetchedMsg{gen: tc.gen, requestSeq: tc.seq, err: &APIError{Code: tc.code}})
			assert.Equal(t, viewDetail, m.view)
			require.NotNil(t, m.detail.issue)
			assert.Equal(t, "current-issue", m.detail.issue.UID)
		})
	}
}

func TestDeleteUndoEntryKeepsQualifiedReference(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[0]
	resp, err := h.client.DeleteIssue(t.Context(), h.project.ID, target.UID, h.qualified(target), "alice")
	require.NoError(t, err)
	require.NotNil(t, resp.undo.entry)
	assert.Equal(t, h.qualified(target), resp.undo.entry.before.QualifiedID)
}

func TestDeleteConfirmationResolvesDetailProject(t *testing.T) {
	for _, source := range []string{"scope", "catalog", "unknown", "qualified"} {
		t.Run(source, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			target := h.issues[0]
			h.selectIssue(target.UID)
			require.Empty(t, h.m.detail.issue.QualifiedID, "daemon detail responses omit the qualified ID")
			switch source {
			case "catalog":
				h.m.scope = scope{allProjects: true}
				h.m.projectsByID = map[int64]string{h.project.ID: h.project.Name}
			case "unknown":
				h.m.scope = scope{allProjects: true}
			case "qualified":
				// List rows supply their own qualified reference, even without a catalog.
				issues, err := h.client.ListIssues(t.Context(), h.project.ID, ListFilter{})
				require.NoError(t, err)
				for _, issue := range issues {
					if issue.UID == target.UID {
						h.m.detail.issue = &issue
					}
				}
				require.Equal(t, h.qualified(target), h.m.detail.issue.QualifiedID)
				h.m.scope = scope{allProjects: true}
			}
			h.key(runeKey('X'))
			if source == "unknown" {
				assert.Equal(t, inputNone, h.m.input.kind)
				require.NotNil(t, h.m.toast)
				assert.Equal(t, "cannot delete: the issue's project is unknown", h.m.toast.text)
				assert.False(t, h.isDeleted(target.UID))
				return
			}
			require.Equal(t, inputDeleteForm, h.m.input.kind)
			assert.Equal(t, h.qualified(target), h.m.input.del.expected)
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			h.finish(h.enter())
			assert.True(t, h.isDeleted(target.UID))
		})
	}
}

func TestMissingBackgroundDetailKeepsForeground(t *testing.T) {
	for _, tc := range []struct {
		name   string
		view   viewID
		layout splitlayout.Mode
	}{
		{"Attention", viewAttention, splitlayout.Stacked},
		{"split Attention", viewAttention, splitlayout.Split},
		{"credentials", viewCredentials, splitlayout.Split},
		{"stacked list", viewList, splitlayout.Stacked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			h.m.layout = tc.layout
			target := h.issues[0]
			h.selectIssue(target.UID)
			h.m.view = tc.view
			_, err := h.client.DeleteIssue(t.Context(), h.project.ID, target.UID, h.qualified(target), "alice")
			require.NoError(t, err)

			h.m = h.drain(h.m.detail.refetch(h.m.api))

			assert.Equal(t, tc.view, h.m.view, "a background refresh keeps the foreground view")
		})
	}
}

func TestDeleteFromSplitListRefetchesOpenParent(t *testing.T) {
	h := newDeleteHarness(t, viewDetail)
	parent, child := h.issues[0], h.issues[1]
	_, err := NewClient(h.env.URL, h.env.HTTP).AddLink(t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(t, err)
	h.selectIssue(parent.UID)
	h.m = h.drain(h.m.detail.refetch(h.m.api))
	require.Len(t, h.m.detail.children, 1)
	h.m.view = viewList
	h.m.layout, h.m.focus = splitlayout.Split, focusList
	h.m.detail.navStack = []detailModel{{scopePID: h.project.ID, issue: &Issue{UID: child.UID}}}
	h.selectIssue(child.UID)

	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(child))
	h.m = h.drain(h.enter())

	require.True(t, h.isDeleted(child.UID))
	require.NotNil(t, h.m.detail.issue)
	assert.Equal(t, parent.UID, h.m.detail.issue.UID)
	assert.Empty(t, h.m.detail.children, "the open parent drops the child deleted from the list")
}
