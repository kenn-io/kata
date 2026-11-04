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
		{"long project at 80 columns", "example-repository-project-with-a-long-task-tracker-name#abc4", 80, 24, false},
		{"typed confirmation in short terminal", strings.Repeat("example-project-", 9) + "#abc4", 80, 11, true},
		{"long qualified id at 80x12", strings.Repeat("example-project-", 15) + "#abc4", 80, 12, false},
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
		{"middle row selects the next row", 1, 2},
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
			assert.Equal(t, "deleted "+h.project.Name+"#"+target.ShortID+"; press u to undo", h.m.toast.text)
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
	for _, tc := range []struct {
		name    string
		view    viewID
		scoped  bool
		context string
	}{
		{name: "unscoped list", view: viewList},
		{name: "unscoped detail", view: viewDetail},
		{name: "scoped list", view: viewList, scoped: true},
		{name: "scoped detail", view: viewDetail, scoped: true},
		{name: "Inbox", view: viewList, context: "Inbox"},
		{name: "Attention", view: viewList, context: "Attention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, tc.view)
			target := h.issues[0]
			if tc.context == "Inbox" {
				_, err := h.env.DB.DesignateInboxProject(t.Context(), db.DesignateInboxProjectIn{ProjectID: h.project.ID, Actor: "alice"})
				require.NoError(t, err)
				h.m = h.drain(h.finish(h.key(runeKey('I'))))
				require.True(t, h.m.scope.inbox)
				require.NotNil(t, h.m.inboxReturn)
			} else if tc.context == "Attention" {
				h.m.attention.returnContext = &attentionReturnContext{detail: h.m.detail}
			}
			h.selectIssue(target.UID)
			if tc.view == viewDetail {
				h.m.focus = focusDetail
				require.NotNil(t, h.m.detail.issue)
			}
			h.m.authCapabilitiesRequired, h.m.authCapabilitiesReady = true, true
			h.m.issueScoped, h.m.scopedWritable = tc.scoped, tc.scoped

			hidden := tc.scoped || tc.context != ""
			hint := helplayout.HelpItem{Key: "X", Description: "delete"}
			if hidden || tc.view == viewDetail {
				assertHelpItemAbsent(t, flattenHelpRows(h.m.helpRows()), hint)
			}
			if hidden {
				assertHelpItemAbsent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
				assertHelpItemAbsent(t, flattenHelpRows(listHelpRows(h.m.list, h.m.chrome())), hint)
			}
			for _, section := range helpSections(h.m.helpKeymap()) {
				for _, item := range section.rows {
					assert.NotEmpty(t, item.Key, "help omits unbound rows")
				}
			}
			helpModel := h.m
			helpModel.view, helpModel.width = viewHelp, 120
			helpText := stripANSI(helpModel.viewBody())
			if hidden {
				assert.NotContains(t, helpText, "delete issue")
			} else {
				assert.Contains(t, helpText, "delete issue")
			}

			cmd := h.key(runeKey('X'))
			if hidden || tc.view == viewDetail {
				assert.Nil(t, cmd)
				assert.Equal(t, inputNone, h.m.input.kind, "X requires list focus and credentials that can delete")
				assert.Equal(t, tc.view, h.m.view)
				assert.Equal(t, target.UID, h.m.list.selectedUID)
				assert.False(t, h.isDeleted(target.UID))
			} else {
				assert.Equal(t, inputDeleteForm, h.m.input.kind)
			}
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
	assert.Equal(t, inputNone, h.m.input.kind)
}

func TestDeleteFromSplitListReplacesRetainedDetail(t *testing.T) {
	t.Run("detail references", func(t *testing.T) {
		deleted := Issue{UID: "deleted-issue"}
		independent := Issue{UID: "independent-issue"}
		prior := detailModel{issue: &Issue{UID: "prior-issue"}}
		for _, tc := range []struct {
			name     string
			detail   detailModel
			preserve bool
		}{
			{name: "deleted issue", detail: detailModel{issue: &deleted, navStack: []detailModel{prior}}},
			{name: "deleted child", detail: detailModel{issue: &independent, children: []Issue{deleted}}},
			{name: "deleted Back entry", detail: detailModel{issue: &independent, navStack: []detailModel{{issue: &deleted}}}},
			{name: "no issue", detail: detailModel{navStack: []detailModel{prior}}},
			{name: "independent issue keeps Back history", detail: detailModel{issue: &independent, navStack: []detailModel{prior}}, preserve: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := tc.detail.withoutIssue(deleted.UID)
				if tc.preserve {
					assert.Equal(t, tc.detail, got)
					assert.Len(t, got.navStack, 1)
				} else {
					assert.Nil(t, got.issue)
					assert.Empty(t, got.children)
					assert.Empty(t, got.navStack)
				}
			})
		}
	})

	t.Run("delete result replaces retained detail", func(t *testing.T) {
		h := newDeleteHarness(t, viewDetail)
		parent, child := h.issues[0], h.issues[1]
		_, err := NewClient(h.env.URL, h.env.HTTP).AddLink(t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
		require.NoError(t, err)
		h.selectIssue(parent.UID)
		h.m = h.drain(h.m.detail.refetch(h.m.api))
		require.Len(t, h.m.detail.children, 1)
		h.m.view = viewList
		h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
		require.Equal(t, splitlayout.Split, h.m.layout)
		h.m.focus = focusList
		h.selectIssue(child.UID)

		h.key(runeKey('X'))
		h.m = typeString(h.m, "DELETE "+h.qualified(child))
		h.m = h.drain(h.enter())

		require.NotNil(t, h.m.detail.issue)
		assert.Equal(t, parent.UID, h.m.detail.issue.UID)
		assert.Empty(t, h.m.detail.children, "the neighbouring parent loads without the deleted child")
	})
}
