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
	t       *testing.T
	env     *testenv.Env
	client  *undoClient
	project db.Project
	issues  []db.Issue
	m       Model
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

// target is the issue under the cursor, selected through the list.
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

func (h *deleteHarness) key(msg tea.KeyPressMsg) tea.Cmd {
	out, cmd := h.m.Update(msg)
	h.m = out.(Model)
	return cmd
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

func TestDeleteFormAsksForTheQualifiedIDAndOnlyEnterOnAnExactMatchDeletes(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)

	h.key(runeKey('X'))
	require.Equal(t, inputDeleteForm, h.m.input.kind, "X opens the Delete form")
	assert.Equal(t, "DELETE "+h.qualified(target), h.m.input.title)
	rendered := stripANSI(renderCenteredForm(h.m.input, 100, 30, h.m.toastNow()))
	assert.Contains(t, rendered, "DELETE "+h.qualified(target))
	assert.Contains(t, rendered, "Second", "the form shows the issue title")

	for _, typed := range []string{"", h.issues[1].ShortID, "DELETE " + h.qualified(target), h.qualified(target)[1:], h.qualified(h.issues[0])} {
		h.m.input.field(fieldConfirm).setValue(typed)
		cmd := h.enter()
		assert.Nil(t, cmd, "enter does nothing for %q", typed)
		assert.Equal(t, inputDeleteForm, h.m.input.kind)
		assert.False(t, h.isDeleted(target.UID), "nothing is deleted for %q", typed)
	}

	h.m.input.field(fieldConfirm).setValue("")
	h.typeText(h.qualified(target))
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
			h.typeText(h.project.Name + "#" + target.ShortID)
			h.finish(h.enter())

			assert.True(t, h.isDeleted(target.UID))
			selected, ok := h.m.list.targetRow()
			require.True(t, ok)
			assert.Equal(t, neighbour.UID, selected.UID, "selection moves to the neighbouring row")
			assert.Equal(t, viewList, h.m.view, "a deleted issue's detail is left")
			require.NotNil(t, h.m.toast)
			assert.Contains(t, h.m.toast.text, "u", "the notice names the undo key")
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
			h.typeText(h.qualified(target))
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
				assert.Equal(t, focusList, h.m.focus)
				require.NotNil(t, h.m.detail.issue, "the pane follows the selected row")
				assert.Equal(t, h.issues[0].UID, h.m.detail.issue.UID)
				assert.Empty(t, h.m.detail.navStack)
			},
		},
		{
			name: "opened from attention",
			setup: func(h *deleteHarness) {
				h.m.attention.returnToAttention = true
			},
			check: func(t *testing.T, h *deleteHarness) {
				assert.Equal(t, viewAttention, h.m.view)
				assert.False(t, h.m.attention.returnToAttention)
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
			h.typeText(h.project.Name + "#" + target.ShortID)
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))

			current, ok := h.m.list.targetRow()
			require.True(t, ok)
			assert.Equal(t, selected.UID, current.UID, "the selection stays on the row that was selected")
			tc.check(t, h)
		})
	}
}
