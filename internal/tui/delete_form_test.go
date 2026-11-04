package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
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

func (h *deleteHarness) typeText(text string) {
	for _, r := range text {
		h.key(runeKey(r))
	}
}

func (h *deleteHarness) enter() tea.Cmd { return h.key(tea.KeyPressMsg{Code: tea.KeyEnter}) }

// finish feeds the daemon's answer to a command back into the model.
func (h *deleteHarness) finish(cmd tea.Cmd) {
	h.t.Helper()
	require.NotNil(h.t, cmd)
	out, _ := h.m.Update(cmd())
	h.m = out.(Model)
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

func TestDeleteFormRequiresFullConfirmation(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)

	h.key(runeKey('X'))
	require.Equal(t, inputDeleteForm, h.m.input.kind, "X opens the Delete form")
	assert.Equal(t, "DELETE "+h.qualified(target), h.m.input.title)
	rendered := stripANSI(renderCenteredForm(h.m.input, 100, 30, h.m.toastNow()))
	assert.Contains(t, rendered, "DELETE "+h.qualified(target))
	assert.Contains(t, rendered, "Type DELETE "+h.qualified(target)+" to confirm.")
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
	h.typeText("DELETE " + h.qualified(target))
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
			h.typeText("DELETE " + h.project.Name + "#" + target.ShortID)
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
			h.typeText("DELETE " + h.qualified(target))
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))
			require.Len(t, h.m.undoHistory.entries, 1)
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
			assert.Contains(t, h.m.toast.text, tc.wantToast)
		})
	}
}

func TestDeleteKeyIsOfferedOnlyToCredentialsThatCanDelete(t *testing.T) {
	hint := helplayout.HelpItem{Key: "X", Description: "delete"}
	for _, scoped := range []bool{false, true} {
		for _, view := range []viewID{viewList, viewDetail} {
			h := newDeleteHarness(t, view)
			h.selectIssue(h.issues[0].UID)
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
				assert.Contains(t, helpText, "delete issue")
			}
			h.m.input = inputState{}
			for _, rows := range [][][]helplayout.HelpItem{listHelpRows(h.m.list, h.m.chrome()), detailHelpRows(h.m.detail, h.m.chrome())} {
				if scoped {
					assertHelpItemAbsent(t, flattenHelpRows(rows), hint)
				} else {
					assertHelpItemPresent(t, flattenHelpRows(rows), hint)
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
			name: "split detail reached by a jump",
			setup: func(h *deleteHarness) {
				h.m.layout = splitlayout.Split
				h.m.focus = focusDetail
			},
			check: func(t *testing.T, h *deleteHarness) {
				assert.Equal(t, focusDetail, h.m.focus)
				require.NotNil(t, h.m.detail.issue, "the pane follows the selected row")
				assert.Equal(t, h.issues[0].UID, h.m.detail.issue.UID)
				assert.Empty(t, h.m.detail.navStack)
			},
		},
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
			name: "opened from attention",
			setup: func(h *deleteHarness) {
				h.m.detail.navStack = nil
				h.m.attention.returnToAttention = true
			},
			check: func(t *testing.T, h *deleteHarness) {
				assert.Equal(t, viewAttention, h.m.view)
				assert.False(t, h.m.attention.returnToAttention)
				require.Contains(t, h.m.toast.text, "press u to undo")
				h.finish(h.key(runeKey('u')))
				assert.False(t, h.isDeleted(h.issues[2].UID))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewDetail)
			rows := h.m.list.visibleRows()
			selected, target := rows[0].issue, rows[2].issue
			h.selectIssue(selected.UID)
			parent := selected
			h.m.detail = detailModel{scopePID: h.project.ID, issue: &target,
				navStack: []detailModel{{issue: &parent}}}
			tc.setup(h)

			h.key(runeKey('X'))
			h.typeText("DELETE " + h.project.Name + "#" + target.ShortID)
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))

			current, ok := h.m.list.targetRow()
			require.True(t, ok)
			assert.Equal(t, selected.UID, current.UID, "the selection stays on the row that was selected")
			tc.check(t, h)
		})
	}
}

func TestDeleteSplitDetailReturnsFocusToList(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	h.m.layout, h.m.focus = splitlayout.Split, focusDetail
	target := h.issues[1]
	h.selectIssue(target.UID)
	detail, err := h.client.GetIssueDetail(t.Context(), h.project.ID, target.UID)
	require.NoError(t, err)
	h.m.detail = detailModel{scopePID: h.project.ID, issue: detail.Issue}
	h.key(runeKey('X'))
	h.typeText("DELETE " + h.qualified(target))
	h.finish(h.enter())
	assert.True(t, h.isDeleted(target.UID))
	assert.Equal(t, focusList, h.m.focus)
	assert.Equal(t, viewList, h.m.view)
	require.NotNil(t, h.m.detail.issue)
	assert.NotEqual(t, target.UID, h.m.detail.issue.UID)
}

func TestDeleteSavingEscKeepsResult(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)
	h.key(runeKey('X'))
	h.typeText("DELETE " + h.qualified(target))
	cmd := h.enter()
	require.NotNil(t, cmd)
	h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.Equal(t, inputDeleteForm, h.m.input.kind)
	h.finish(cmd)
	assert.True(t, h.isDeleted(target.UID))
	assert.Equal(t, inputNone, h.m.input.kind)
	for _, row := range h.m.list.visibleRows() {
		assert.NotEqual(t, target.UID, row.issue.UID)
	}
	require.NotNil(t, h.m.toast)
	assert.Contains(t, h.m.toast.text, "deleted "+h.qualified(target)+"; press u to undo")
}

func TestDeleteRestoreEventsRefreshLabelCounts(t *testing.T) {
	for _, eventType := range []string{"issue.soft_deleted", "issue.restored"} {
		t.Run(eventType, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			target := h.issues[0]
			base := NewClient(h.env.URL, h.env.HTTP)
			_, err := base.AddLabel(t.Context(), h.project.ID, target.UID, "bug", "alice")
			require.NoError(t, err)
			if eventType == "issue.restored" {
				_, err = base.DeleteIssue(t.Context(), h.project.ID, target.UID, h.qualified(target), "alice")
				require.NoError(t, err)
			}
			var cmd tea.Cmd
			h.m, cmd = h.m.dispatchLabelFetch(h.project.ID)
			h.finish(cmd)
			if eventType == "issue.soft_deleted" {
				require.Equal(t, []LabelCount{{Label: "bug", Count: 1}}, h.m.projectLabels.byProject[h.project.ID].labels)
				_, err = base.DeleteIssue(t.Context(), h.project.ID, target.UID, h.qualified(target), "alice")
			} else {
				require.Empty(t, h.m.projectLabels.byProject[h.project.ID].labels)
				_, err = base.RestoreIssue(t.Context(), h.project.ID, target.UID, "alice")
			}
			require.NoError(t, err)
			h.m, cmd = h.m.maybeRefetchLabels(eventReceivedMsg{eventType: eventType, projectID: h.project.ID})
			require.NotNil(t, cmd, "the lifecycle event refetches cached label counts")
			h.finish(cmd)
			if eventType == "issue.soft_deleted" {
				assert.Empty(t, h.m.projectLabels.byProject[h.project.ID].labels)
			} else {
				assert.Equal(t, []LabelCount{{Label: "bug", Count: 1}}, h.m.projectLabels.byProject[h.project.ID].labels)
			}
		})
	}
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
			assert.Equal(t, "DELETE "+h.qualified(target), h.m.input.title)
			h.typeText("DELETE " + h.qualified(target))
			h.finish(h.enter())
			assert.True(t, h.isDeleted(target.UID))
		})
	}
}
