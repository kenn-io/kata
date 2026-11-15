package tui

import (
	"encoding/json/jsontext"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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
	useInstantTicks(t)
	ctx := t.Context()
	env := testenv.New(t)
	project, err := env.DB.CreateProject(ctx, "example-project")
	require.NoError(t, err)
	h := &deleteHarness{t: t, env: env, project: project,
		client: newConnectedUndoClient(t, NewClient(env.URL, env.HTTP))}
	for _, title := range []string{"First", "Second", "Third"} {
		issue, _, err := env.DB.CreateIssue(ctx, db.CreateIssueParams{
			ProjectID: project.ID, Title: title, Author: "alice",
			Metadata: map[string]jsontext.Value{notification.MetadataKey("alice"): jsontext.Value(`{"from":"bob","message":"Review"}`)},
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
	var follow tea.Cmd
	h.m, follow = stepModel(h.m, cmd())
	return follow
}

// drain follows fetch results and their Back commands until navigation settles.
func (h *deleteHarness) drain(cmd tea.Cmd) Model {
	h.t.Helper()
	for _, msg := range runCmdTree(cmd) {
		next, follow := updateModel(h.m, msg)
		h.m = next
		switch msg.(type) {
		case attentionLoadedMsg, detailFollowTickMsg, detailFetchedMsg, commentsFetchedMsg, eventsFetchedMsg, linksFetchedMsg, openDetailMsg, jumpDetailMsg, popDetailMsg, mutationDoneMsg, undoDoneMsg:
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

// openInbox designates the project as the Inbox and switches to it.
func (h *deleteHarness) openInbox() {
	h.t.Helper()
	_, err := h.env.DB.DesignateInboxProject(h.t.Context(), db.DesignateInboxProjectIn{ProjectID: h.project.ID, Actor: "alice"})
	require.NoError(h.t, err)
	h.m = enterInboxForTest(h.t, h.m)
}

// openAttentionDetail opens the issue's detail from Attention.
func (h *deleteHarness) openAttentionDetail(uid string) {
	h.t.Helper()
	h.m = h.drain(h.key(runeKey('T')))
	h.m.attention.cursor = slices.IndexFunc(h.m.attention.rows, func(row attentionRequest) bool { return row.issue.UID == uid })
	require.NotEqual(h.t, -1, h.m.attention.cursor)
	h.m = h.drain(h.enter())
}

// focusChild opens parent with child linked under it and focuses the children.
func (h *deleteHarness) focusChild(parent, child db.Issue) {
	h.t.Helper()
	_, err := h.client.KataAPI.AddLink(h.t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
	require.NoError(h.t, err)
	h.selectIssue(parent.UID)
	h.m = h.drain(h.m.detail.refetch(h.m.api))
	h.m.detail.detailFocus = focusChildren
}

func TestDeletePreservesPendingReturn(t *testing.T) {
	for _, destination := range []string{"Inbox", "Attention"} {
		for _, deletedTarget := range []string{"unrelated target", "pending child"} {
			t.Run(destination+"/"+deletedTarget, func(t *testing.T) {
				t.Setenv("KATA_INBOX_USER", "alice")
				h := newDeleteHarness(t, viewDetail)
				parent, child, target := h.issues[0], h.issues[1], h.issues[2]
				if deletedTarget == "pending child" {
					target = child
				}
				h.focusChild(parent, child)
				pending := h.finish(h.enter())
				defer h.m.detail.cancelReads()
				require.Nil(t, h.m.detail.issue)

				if destination == "Inbox" {
					h.openInbox()
					h.selectIssue(target.UID)
				} else {
					h.openAttentionDetail(target.UID)
				}
				h.key(runeKey('X'))
				h.m = typeString(h.m, "DELETE "+h.qualified(target))
				h.m = h.drain(h.enter())
				require.True(t, h.isDeleted(target.UID))
				if destination == "Inbox" {
					h.m = h.drain(h.key(runeKey('I')))
				} else {
					h.m = h.drain(h.key(tea.KeyPressMsg{Code: tea.KeyEscape}))
				}
				require.Equal(t, viewDetail, h.m.view, "return keeps the detail open")
				if deletedTarget == "pending child" {
					require.NotNil(t, h.m.detail.issue)
					require.Equal(t, parent.UID, h.m.detail.issue.UID, "return reaches the surviving parent")
					h.m = h.drain(pending)
					require.NotNil(t, h.m.detail.issue)
					assert.Equal(t, parent.UID, h.m.detail.issue.UID, "late child results keep the parent open")
					return
				}
				h.m = h.drain(pending)
				require.NotNil(t, h.m.detail.issue)
				require.Equal(t, child.UID, h.m.detail.issue.UID)
				h.m = h.drain(h.key(tea.KeyPressMsg{Code: tea.KeyEscape}))
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, parent.UID, h.m.detail.issue.UID, "Back still reaches the parent")
			})
		}
	}
}

func TestDeletePreservesIndependentSplitDetail(t *testing.T) {
	for _, state := range []string{"resolved", "pending"} {
		t.Run(state, func(t *testing.T) {
			h := newDeleteHarness(t, viewList)
			rows := h.m.list.visibleRows()
			parent, child := rows[0].issue, rows[2].issue
			_, err := h.client.KataAPI.AddLink(t.Context(), h.project.ID, child.UID, LinkBody{Type: "parent", ToRef: parent.UID}, "alice")
			require.NoError(t, err)

			h.selectIssue(parent.UID)
			m, cmd := updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
			h.m = m
			if state == "resolved" {
				h.m = h.drain(cmd)
			}
			require.Equal(t, splitlayout.Split, h.m.layout)
			h.m = h.drain(h.enter())
			h.m.detail.detailFocus = focusChildren
			pending := h.finish(h.enter())
			defer h.m.detail.cancelReads()
			var late []tea.Msg
			if state == "resolved" {
				h.m = h.drain(pending)
				require.NotNil(t, h.m.detail.issue)
				require.Equal(t, child.UID, h.m.detail.issue.UID)
			} else {
				require.Nil(t, h.m.detail.issue)
				late = runCmdTree(pending)
			}
			require.Len(t, h.m.detail.navStack, 1)

			h.m.focus = focusList
			h.key(runeKey('X'))
			require.Equal(t, inputDeleteForm, h.m.input.kind)
			h.m = typeString(h.m, "DELETE "+h.project.Name+"#"+parent.ShortID)
			refresh := h.finish(h.enter())
			require.True(t, h.isDeleted(parent.UID))
			if state == "resolved" {
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, child.UID, h.m.detail.issue.UID, "deletion keeps the independently opened detail")
			}
			h.m = h.drain(refresh)
			for _, msg := range late {
				h.m, _ = updateModel(h.m, msg)
			}
			require.NotNil(t, h.m.detail.issue)
			assert.Equal(t, child.UID, h.m.detail.issue.UID, "list refresh keeps the requested child detail")
		})
	}
}

func TestAttentionUndoIgnoresOtherEdits(t *testing.T) {
	h := newDeleteHarness(t, viewAttention)
	h.m.undoHistory.push(undoEntry{kind: "close", uid: h.issues[0].UID, projectID: h.project.ID})
	assertHelpItemAbsent(t, flattenHelpRows(attentionHelpRows(h.m)), helplayout.HelpItem{Key: "u", Description: "undo"})
	assert.Nil(t, h.key(runeKey('u')))
	assert.Len(t, h.m.undoHistory.entries, 1)
}

// A delete must clear every place Back or a view switch can reveal.
func TestDeleteReachesEverySavedView(t *testing.T) {
	for _, context := range []string{"Inbox", "Attention detail", "Attention Back", "split detail"} {
		t.Run(context, func(t *testing.T) {
			t.Setenv("KATA_INBOX_USER", "alice")
			h := newDeleteHarness(t, viewDetail)
			parent, target := h.issues[0], h.issues[1]
			h.selectIssue(target.UID)
			h.m = h.drain(h.m.detail.refetch(h.m.api))
			require.Empty(t, h.m.detail.issue.QualifiedID, "confirmation must work with a fetched detail")
			h.m.focus = focusDetail
			switch context {
			case "Inbox":
				h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
				require.Equal(t, splitlayout.Split, h.m.layout)
				h.openInbox()
				require.True(t, h.m.scope.inbox)
				h.selectIssue(target.UID)
			case "Attention detail", "Attention Back":
				if context == "Attention Back" {
					h.focusChild(parent, target)
					h.m = h.drain(h.enter())
				}
				h.openAttentionDetail(target.UID)
			case "split detail":
				h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
				require.Equal(t, splitlayout.Split, h.m.layout)
				h.m.focus = focusDetail
			}

			h.key(runeKey('X'))
			require.Equal(t, inputDeleteForm, h.m.input.kind, "X opens delete in this view")
			h.m = typeString(h.m, "DELETE "+h.qualified(target))
			refresh := h.finish(h.enter())
			assertNoDeletedView(t, h.m, target.UID)
			if context == "split detail" {
				assert.Equal(t, focusList, h.m.focus, "delete returns focus to the list before refresh")
				selected, ok := h.m.list.targetRow()
				require.True(t, ok)
				require.NotNil(t, h.m.detail.issue, "delete starts loading the neighbouring row before refresh")
				assert.Equal(t, selected.UID, h.m.detail.issue.UID)
			}
			if context == "Attention detail" {
				require.Len(t, h.m.attention.rows, 2)
				h.m = h.drain(h.enter())
				require.NotNil(t, h.m.detail.issue)
				assert.NotEqual(t, target.UID, h.m.detail.issue.UID, "Enter cannot reopen the removed row before refresh")
				h.m = h.drain(h.key(tea.KeyPressMsg{Code: tea.KeyEscape}))
			} else {
				h.m = h.drain(refresh)
			}
			require.True(t, h.isDeleted(target.UID))
			assertNoDeletedView(t, h.m, target.UID)
			switch context {
			case "Inbox":
				h.m = h.drain(h.key(tea.KeyPressMsg{Code: tea.KeyEscape}))
				assert.Equal(t, viewList, h.m.view)
				selected, ok := h.m.list.targetRow()
				require.True(t, ok)
				require.NotNil(t, h.m.detail.issue, "leaving Inbox loads the highlighted neighbour")
				assert.Equal(t, selected.UID, h.m.detail.issue.UID)
			case "Attention detail", "Attention Back":
				assert.Equal(t, viewAttention, h.m.view)
				assertHelpItemPresent(t, flattenHelpRows(attentionHelpRows(h.m)), helplayout.HelpItem{Key: "u", Description: "undo"})
			case "split detail":
				selected, ok := h.m.list.targetRow()
				require.True(t, ok)
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, selected.UID, h.m.detail.issue.UID)
			}
			if context == "Attention detail" || context == "Attention Back" {
				h.m = h.drain(h.key(runeKey('u')))
				assert.False(t, h.isDeleted(target.UID))
				assert.True(t, slices.ContainsFunc(h.m.attention.rows, func(row attentionRequest) bool { return row.issue.UID == target.UID }), "undo reloads Attention")
			}
			if context == "Attention Back" {
				h.m = h.drain(h.key(tea.KeyPressMsg{Code: tea.KeyEscape}))
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, parent.UID, h.m.detail.issue.UID, "the saved deleted detail pops Back to its parent")
			}
		})
	}
}

// Returning after a delete must refetch the saved parent and reject older reads.
func TestDeleteInvalidatesSavedRelations(t *testing.T) {
	for _, route := range []string{"Back", "Inbox list", "Attention list"} {
		t.Run(route, func(t *testing.T) {
			t.Setenv("KATA_INBOX_USER", "alice")
			h := newDeleteHarness(t, viewDetail)
			parent, child := h.issues[0], h.issues[1]
			_, err := h.client.KataAPI.AddLink(t.Context(), h.project.ID, parent.UID, LinkBody{Type: "related", ToRef: child.UID}, "alice")
			require.NoError(t, err)
			h.focusChild(parent, child)
			require.NotEmpty(t, h.m.detail.links)
			late := runCmdTree(h.m.detail.refetch(h.m.api))
			switch route {
			case "Back":
				h.m = h.drain(h.enter())
			case "Inbox list":
				h.m.view = viewList
				h.openInbox()
				h.selectIssue(child.UID)
			case "Attention list":
				h.m.view = viewList
				h.openAttentionDetail(child.UID)
			}
			h.key(runeKey('X'))
			require.Equal(t, inputDeleteForm, h.m.input.kind)
			h.m = typeString(h.m, "DELETE "+h.qualified(child))
			deleted := h.finish(h.enter())
			assertNoDeletedView(t, h.m, child.UID)
			var refresh tea.Cmd
			switch route {
			case "Back":
				refresh = deleted
			case "Inbox list":
				refresh = h.key(runeKey('I'))
			case "Attention list":
				refresh = h.key(tea.KeyPressMsg{Code: tea.KeyEscape})
			}
			for _, msg := range late {
				h.m, _ = updateModel(h.m, msg)
			}
			assertNoDeletedView(t, h.m, child.UID)
			assert.NotContains(t, stripANSI(h.m.detail.View(120, 40, h.m.chrome())), "Second")
			h.m = h.drain(refresh)
			switch route {
			case "Inbox list", "Attention list":
				require.Equal(t, viewList, h.m.view)
			case "Back":
				require.Equal(t, viewDetail, h.m.view)
				require.NotNil(t, h.m.detail.issue)
				assert.Equal(t, parent.UID, h.m.detail.issue.UID)
				assert.Empty(t, h.m.detail.children)
				h.m = h.drain(h.key(runeKey('u')))
				require.Len(t, h.m.detail.children, 1, "undo puts the child back in the open parent")
			}
		})
	}
}

func assertNoDeletedView(t *testing.T, m Model, uid string) {
	t.Helper()
	var checkDetail func(dm detailModel)
	checkDetail = func(dm detailModel) {
		if dm.issue != nil {
			assert.NotEqual(t, uid, dm.issue.UID)
		}
		for _, link := range dm.links {
			assert.NotEqual(t, uid, link.From.UID)
			assert.NotEqual(t, uid, link.To.UID)
		}
		if dm.parent != nil {
			assert.NotEqual(t, uid, dm.parent.UID)
		}
		for _, child := range dm.children {
			assert.NotEqual(t, uid, child.UID)
		}
		for _, prev := range dm.navStack {
			checkDetail(prev)
		}
	}
	checkDetail(m.detail)
	for _, row := range m.attention.rows {
		assert.NotEqual(t, uid, row.issue.UID)
	}
	if m.inboxReturn != nil {
		checkDetail(m.inboxReturn.detail)
		for _, row := range m.inboxReturn.list.visibleRows() {
			assert.NotEqual(t, uid, row.issue.UID)
		}
	}
	if m.attention.returnContext != nil {
		checkDetail(m.attention.returnContext.detail)
	}
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
		name     string
		view     viewID
		scoped   bool
		unloaded bool
	}{
		{name: "unscoped list", view: viewList},
		{name: "unscoped detail", view: viewDetail},
		{name: "scoped list", view: viewList, scoped: true},
		{name: "scoped detail", view: viewDetail, scoped: true},
		{name: "detail with no issue loaded", view: viewDetail, unloaded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDeleteHarness(t, tc.view)
			target := h.issues[0]
			h.selectIssue(target.UID)
			if tc.view == viewDetail {
				h.m.focus = focusDetail
				require.NotNil(t, h.m.detail.issue)
				if tc.unloaded {
					h.m.detail = newDetailModel()
				}
			}
			h.m.authCapabilitiesRequired, h.m.authCapabilitiesReady = true, true
			h.m.issueScoped, h.m.scopedWritable = tc.scoped, tc.scoped

			hint := helplayout.HelpItem{Key: "X", Description: "delete"}
			if tc.scoped || tc.unloaded {
				assertHelpItemAbsent(t, flattenHelpRows(h.m.helpRows()), hint)
				assertHelpItemAbsent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
			} else {
				assertHelpItemPresent(t, flattenHelpRows(h.m.helpRows()), hint)
				assertHelpItemPresent(t, flattenHelpRows(h.m.splitHelpRows()), hint)
			}
			for _, section := range helpSections(h.m.helpKeymap()) {
				for _, item := range section.rows {
					assert.NotEmpty(t, item.Key, "help omits unbound rows")
				}
			}
			helpModel := h.m
			helpModel.view, helpModel.width = viewHelp, 120
			helpText := stripANSI(helpModel.viewBody())
			if tc.scoped {
				assertHelpItemAbsent(t, flattenHelpRows(listHelpRows(h.m.list, h.m.chrome())), hint)
				assert.NotContains(t, helpText, "delete issue")
			} else {
				assert.Contains(t, helpText, "delete issue")
			}

			cmd := h.key(runeKey('X'))
			if tc.scoped || tc.unloaded {
				assert.Nil(t, cmd)
				assert.Equal(t, inputNone, h.m.input.kind, "X requires delete credentials and a loaded issue")
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
	h := newDeleteHarness(t, viewDetail)
	parent, child := h.issues[0], h.issues[1]
	h.focusChild(parent, child)
	require.Len(t, h.m.detail.children, 1)
	h.m.view = viewList
	h.m, _ = updateModel(h.m, tea.WindowSizeMsg{Width: 180, Height: 40})
	require.Equal(t, splitlayout.Split, h.m.layout)
	h.m.focus = focusList
	back := h.m.list.issues[slices.IndexFunc(h.m.list.issues, func(issue Issue) bool { return issue.UID == h.issues[2].UID })]
	h.m.detail.navStack = []detailModel{{scopePID: h.project.ID, issue: &back}}
	h.selectIssue(child.UID)
	late := runCmdTree(h.m.detail.refetch(h.m.api))

	h.key(runeKey('X'))
	h.m = typeString(h.m, "DELETE "+h.qualified(child))
	refresh := h.finish(h.enter())

	assert.Empty(t, h.m.detail.children, "the deleted child disappears before refresh")
	for _, msg := range late {
		h.m, _ = updateModel(h.m, msg)
	}
	assert.Empty(t, h.m.detail.children, "earlier reads cannot restore the deleted child")
	h.m = h.drain(refresh)

	assert.Empty(t, h.m.detail.children, "the retained parent refreshes without the deleted child")
}
