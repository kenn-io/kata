package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	for _, title := range []string{"First", "Second", "Third"} {
		issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: title, Author: "alice",
		})
		require.NoError(t, err)
		h.issues = append(h.issues, issue)
	}
	rows, err := h.client.ListIssues(ctx, project.ID, ListFilter{})
	require.NoError(t, err)
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
func (h *deleteHarness) finish(cmd tea.Cmd) tea.Cmd {
	h.t.Helper()
	require.NotNil(h.t, cmd)
	out, follow := h.m.Update(cmd())
	h.m = out.(Model)
	return follow
}

// drain follows fetch results and their Back commands until navigation settles.
func (h *deleteHarness) drain(cmd tea.Cmd) Model {
	h.t.Helper()
	for _, msg := range runCmdTree(cmd) {
		next, follow := updateModel(h.m, msg)
		h.m = next
		switch msg.(type) {
		case detailFollowTickMsg, detailFetchedMsg, commentsFetchedMsg, eventsFetchedMsg, linksFetchedMsg, openDetailMsg, jumpDetailMsg, popDetailMsg, mutationDoneMsg, undoDoneMsg:
			h.m = h.drain(follow)
		}
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
	defer snapshotInit(t)()
	for _, tc := range []struct {
		name      string
		qualified string
		width     int
		height    int
		typed     bool
	}{
		{"long project in short terminal", "example-repository-project-with-a-very-long-task-tracker-name-that-exceeds-eighty-columns#abc4", 80, 11, false},
		{"80 columns", "example-repository-project-task-tracker#abc4", 80, 24, false},
		{"long project at 80 columns", "example-repository-project-with-a-long-task-tracker-name#abc4", 80, 24, false},
		{"typed confirmation in short terminal", strings.Repeat("example-project-", 9) + "#abc4", 80, 11, true},
		{"long title at 80x12", strings.Repeat("example-project-", 15) + "#abc4", 80, 12, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			form := newDeleteForm(formTarget{}, Issue{Title: "Duplicate issue"}, tc.qualified)
			if tc.typed {
				form.field(fieldConfirm).setValue("DELETE " + tc.qualified)
			}
			rendered := stripANSI(renderCenteredForm(form, tc.width, tc.height, snapshotFixedNow))
			var content strings.Builder
			for line := range strings.SplitSeq(rendered, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), tc.width, "each line fits the terminal")
				content.WriteString(strings.Trim(line, " │╭╮╰╯─"))
			}
			confirmation := strings.Join(strings.Fields(content.String()), "")
			wantCount := 1
			if tc.typed {
				wantCount = 2
			}
			assert.Equal(t, wantCount, strings.Count(confirmation, "DELETE"+tc.qualified), "the full confirmation remains visible across wrapped lines:\n%s", rendered)
			if tc.height >= formMinHeight {
				assert.Contains(t, rendered, "Duplicate issue", "the form shows the issue title")
				assert.LessOrEqual(t, strings.Count(rendered, "\n")+1, tc.height, "the whole modal fits:\n%s", rendered)
				m := initialModel(Options{})
				m.width, m.height, m.input = tc.width, tc.height, form
				visible := stripANSI(m.overlayInputAndModal(strings.Repeat(strings.Repeat(" ", tc.width)+"\n", tc.height-1)))
				assert.Contains(t, visible, "Confirm")
				assert.Contains(t, visible, "enter delete · esc cancel")
			}
		})
	}
}

func TestDeleteFormRequiresFullConfirmation(t *testing.T) {
	h := newDeleteHarness(t, viewList)
	target := h.issues[1]
	h.selectIssue(target.UID)

	h.key(runeKey('X'))
	require.Equal(t, inputDeleteForm, h.m.input.kind, "X opens the Delete form")

	for _, typed := range []string{"", h.issues[1].ShortID, h.qualified(target), h.qualified(target)[1:], "DELETE " + h.qualified(h.issues[0]), "delete " + h.qualified(target)} {
		h.m.input.field(fieldConfirm).setValue(typed)
		cmd := h.enter()
		assert.Nil(t, cmd, "enter does nothing for %q", typed)
		assert.Equal(t, inputDeleteForm, h.m.input.kind)
		assert.False(t, h.isDeleted(target.UID), "nothing is deleted for %q", typed)
	}

	h.m.input.field(fieldConfirm).setValue("")
	h.m = typeString(h.m, "  DELETE "+h.qualified(target)+"  ")
	h.finish(h.enter())
	assert.Equal(t, inputNone, h.m.input.kind, "a delete with surrounding spaces closes the form: "+h.m.input.err)
	assert.True(t, h.isDeleted(target.UID))
}

func TestDeleteSelectsTheNeighbouringRowAndNamesUndo(t *testing.T) {
	cases := []struct {
		name              string
		target, neighbour int
	}{
		{"list", 1, 2},
		{"last row selects the previous row", 2, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			rows := h.m.list.visibleRows()
			require.Len(t, rows, 3)
			target, neighbour := rows[tc.target].issue, rows[tc.neighbour].issue
			h.selectIssue(target.UID)

			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.project.Name+"#"+target.ShortID)
			h.finish(h.enter())

			assert.True(t, h.isDeleted(target.UID))
			for _, row := range h.m.list.visibleRows() {
				assert.NotEqual(t, target.UID, row.issue.UID, "the confirmed issue leaves the list")
			}
			selected, ok := h.m.list.targetRow()
			require.True(t, ok)
			assert.Equal(t, neighbour.UID, selected.UID, "selection moves to the neighbouring row")
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
		{name: "restores the issue"},
		{name: "reports an issue that is no longer deleted", restoredElsewhere: true, wantToast: "already restored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			target := h.issues[0]
			_, err := NewClient(h.env.URL, h.env.HTTP).AddLabel(t.Context(), h.project.ID, target.UID, "urgent", "alice")
			require.NoError(t, err)
			detail, err := h.client.GetIssueDetail(t.Context(), h.project.ID, target.UID)
			require.NoError(t, err)
			require.Empty(t, detail.Issue.QualifiedID, "the undo preflight needs the list's qualified reference")
			h.selectIssue(target.UID)
			h.key(runeKey('X'))
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			h.finish(h.enter())
			require.True(t, h.isDeleted(target.UID))
			require.Len(t, h.m.undoHistory.entries, 1)
			if tc.restoredElsewhere {
				_, _, changed, err := h.env.DB.RestoreIssue(t.Context(), target.ID, "bob")
				require.NoError(t, err)
				require.True(t, changed)
			}

			h.m.projectLabels = newLabelCache()
			h.m.projectLabels.byProject[h.project.ID] = labelCacheEntry{pid: h.project.ID}
			refresh := h.finish(h.key(runeKey('u')))

			assert.False(t, h.isDeleted(target.UID))
			assert.Empty(t, h.m.undoHistory.entries, "the spent entry is dropped")
			require.NotNil(t, h.m.toast)
			assert.Equal(t, toastInfo, h.m.toast.level, "an already restored issue is not an error")
			if tc.restoredElsewhere {
				assert.Contains(t, h.m.toast.text, tc.wantToast)
			} else {
				assert.Equal(t, "undid delete on "+h.qualified(target), h.m.toast.text)
			}
			h.m = h.drain(refresh)
			assert.Equal(t, []LabelCount{{Label: "urgent", Count: 1}}, h.m.projectLabels.byProject[h.project.ID].labels, "undo refreshes the restored issue's project labels")
		})
	}
}

func TestDeleteKeyIsOfferedOnlyToCredentialsThatCanDelete(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, view := range []viewID{viewList, viewDetail, viewHelp} {
			h := newDeleteHarness(t, viewList)
			h.selectIssue(h.issues[0].UID)
			if view == viewHelp {
				h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
				require.Equal(t, splitlayout.Split, h.m.layout)
				require.Equal(t, focusList, h.m.focus)
				h.key(runeKey('?'))
				require.Equal(t, viewHelp, h.m.view)
			} else {
				h.m.view = view
			}
			if view == viewDetail {
				h.m.focus = focusDetail
			}
			h.m.authCapabilitiesRequired, h.m.authCapabilitiesReady = true, true
			h.m.issueScoped, h.m.scopedWritable = scoped, scoped

			if view != viewHelp {
				hint := helplayout.HelpItem{Key: "X", Description: "delete"}
				if scoped || view == viewDetail {
					assertHelpItemAbsent(t, flattenHelpRows(h.m.helpRows()), hint)
					assertHelpItemAbsent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
				} else {
					assertHelpItemPresent(t, flattenHelpRows(h.m.helpRows()), hint)
					assertHelpItemPresent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
				}
			}

			cmd := h.key(runeKey('X'))
			helpText := stripANSI(renderHelp(h.m.helpKeymap(), 120, ListFilter{}))
			if scoped || view != viewList {
				assert.Nil(t, cmd)
				assert.Equal(t, inputNone, h.m.input.kind, "X requires list focus and credentials that can delete")
				assert.Equal(t, view, h.m.view)
				assert.False(t, h.isDeleted(h.issues[0].UID))
				if scoped {
					assert.NotContains(t, helpText, "delete issue")
				}
			} else {
				assert.Equal(t, inputDeleteForm, h.m.input.kind)
			}
		}
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
	assert.Equal(t, inputNone, h.m.input.kind)
	for _, row := range h.m.list.visibleRows() {
		assert.NotEqual(t, target.UID, row.issue.UID, "the confirmed issue leaves the list")
	}
	require.NotNil(t, h.m.toast)
	assert.Equal(t, "deleted "+h.qualified(target)+"; press u to undo", h.m.toast.text)
}

func TestDeleteFromSplitListReplacesRetainedDetail(t *testing.T) {
	h := newDeleteHarness(t, viewDetail)
	parent, child := h.issues[0], h.issues[1]
	_, err := NewClient(h.env.URL, h.env.HTTP).AddLink(t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(t, err)
	h.selectIssue(parent.UID)
	h.m = h.drain(h.m.detail.refetch(h.m.api))
	require.Len(t, h.m.detail.children, 1)
	h.m.view = viewList
	h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
	h.m.focus = focusList
	h.m.detail.navStack = []detailModel{{scopePID: h.project.ID, issue: &Issue{UID: child.UID}}}
	h.selectIssue(child.UID)

	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(child))
	h.m = h.drain(h.enter())

	require.True(t, h.isDeleted(child.UID))
	require.NotNil(t, h.m.detail.issue)
	assert.Equal(t, parent.UID, h.m.detail.issue.UID)
	assert.False(t, h.m.detail.loading, "the neighbouring parent's detail read completes")
	assert.Equal(t, parent.Title, h.m.detail.issue.Title)
	assert.NotEmpty(t, h.m.detail.events, "the neighbouring parent's activity loads")
	assert.Empty(t, h.m.detail.children, "the neighbouring parent loads without the deleted child")
	assert.Empty(t, h.m.detail.navStack, "the replacement detail starts fresh")
}

func TestDeleteKeyDoesNothingWithSavedReturnContext(t *testing.T) {
	for _, context := range []string{"Inbox", "Attention"} {
		t.Run(context, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			target := h.issues[0]
			if context == "Inbox" {
				_, err := h.env.DB.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{ProjectID: h.project.ID, Actor: "alice"})
				require.NoError(t, err)
				h.m = h.drain(h.finish(h.key(runeKey('I'))))
				require.True(t, h.m.scope.inbox)
				require.NotNil(t, h.m.inboxReturn)
			} else {
				h.m.attention.returnContext = &attentionReturnContext{detail: h.m.detail}
			}
			h.selectIssue(target.UID)

			hint := helplayout.HelpItem{Key: "X", Description: "delete"}
			assertHelpItemAbsent(t, flattenHelpRows(h.m.helpRows()), hint)
			assertHelpItemAbsent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
			assertHelpItemAbsent(t, flattenHelpRows(listHelpRows(h.m.list, h.m.chrome())), hint)
			assert.NotContains(t, stripANSI(renderHelp(h.m.helpKeymap(), 120, ListFilter{})), "delete issue")

			cmd := h.key(runeKey('X'))

			assert.Nil(t, cmd)
			assert.Equal(t, inputNone, h.m.input.kind, "X leaves the delete form closed")
			assert.Equal(t, viewList, h.m.view)
			assert.Equal(t, target.UID, h.m.list.selectedUID)
			assert.False(t, h.isDeleted(target.UID))
		})
	}
}
