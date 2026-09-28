package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestSplit_CloseOpenFilterFollowsRemainingIssue(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()

	first := Issue{ProjectID: 7, UID: "01TEST-first", ShortID: "f001", Title: "first issue", Status: "open"}
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{first, closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.cursor = 1
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = closing.ProjectID
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.detail.gen = 41
	m.nextGen = 41
	m.focus = focusDetail

	closed := closing
	closed.Status = "closed"
	after, cmd := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{first, closed, remaining},
	})
	require.Equal(t, remaining.UID, after.list.selectedUID)
	require.NotNil(t, after.detail.issue)
	require.Equal(t, remaining.UID, after.detail.issue.UID)
	require.NotNil(t, cmd)
}

func TestSplit_CloseOpenFilterClearsDetailWhenNoIssuesRemain(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()

	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	m.list.issues = []Issue{closing}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = closing.ProjectID
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.nextDetailFollowGen = 4

	closed := closing
	closed.Status = "closed"
	after, _ := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed},
	})
	require.Empty(t, after.list.selectedUID)
	require.Nil(t, after.detail.issue)
	require.Equal(t, int64(5), after.nextDetailFollowGen)

	after, cmd := updateModel(after, detailFollowTickMsg{gen: 4})
	require.Nil(t, after.detail.issue)
	require.Nil(t, cmd)
}

func TestSplit_InitialRetryFollowsReplacementIssue(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()

	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	replacement := Issue{ProjectID: 7, UID: "01TEST-next", ShortID: "n003", Title: "next issue", Status: "open"}
	m.list.issues = []Issue{closing, replacement}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = closing.ProjectID
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.detail.gen = 41
	m.nextGen = 41

	closed := closing
	closed.Status = "closed"
	after, cmd := updateModel(m, initialFetchMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed, replacement},
	})
	require.Equal(t, replacement.UID, after.list.selectedUID)
	require.NotNil(t, after.detail.issue)
	require.Equal(t, replacement.UID, after.detail.issue.UID)
	require.NotNil(t, cmd)
}

func TestSplit_InitialFetchPreservesIssueSeedWithoutPriorSelection(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	seeded := Issue{ProjectID: 7, UID: "01TEST-seeded", ShortID: "seed", Title: "explicit issue"}
	m.list.issues = nil
	m.list.cursor = 0
	m.list.selectedUID = ""
	m.list.selectedProjectID = 0
	m.detail.issue = &seeded // --issue pins this detail before the list fetch.
	m.detail.scopePID = 7

	after, cmd := updateModel(m, initialFetchMsg{
		dispatchKey: m.currentCacheKey(),
		issues: []Issue{
			{ProjectID: 7, UID: "01TEST-default", ShortID: "dflt", Title: "default highlighted row"},
		},
	})

	require.NotNil(t, after.detail.issue)
	require.Equal(t, "seed", after.detail.issue.ShortID)
	require.Nil(t, cmd, "the list's default row must not retarget explicit --issue detail")
}

func TestSplit_FilterFormCommitFollowsResetSelection(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	open := Issue{ProjectID: 7, UID: "01TEST-open", ShortID: "open", Title: "open issue", Status: "open"}
	closed := Issue{ProjectID: 7, UID: "01TEST-closed", ShortID: "clos", Title: "closed issue", Status: "closed"}
	m.list.issues = []Issue{open, closed}
	m.list.selectedUID = open.UID
	m.list.selectedProjectID = open.ProjectID
	m.detail.issue = &open
	m.detail.scopePID = 7
	m.input = newFilterForm(m.list.filter, m.scope)
	m.input.field(fieldStatus).setValue("closed")

	after, cmd := m.commitInput()

	require.Equal(t, inputNone, after.input.kind)
	require.Equal(t, "closed", after.list.filter.Status)
	require.Equal(t, closed.UID, after.detail.issue.UID)
	require.NotNil(t, cmd, "the newly highlighted filtered issue must schedule a detail fetch")
}

func TestSplit_DismissingInputKeepsDrilledDetailWithoutDeferredSelectionChange(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	parent := Issue{ProjectID: 7, UID: "01TEST-parent", ShortID: "par1", Title: "parent"}
	child := Issue{ProjectID: 7, UID: "01TEST-child", ShortID: "chi2", Title: "child"}
	m.list.issues = []Issue{parent, child}
	m.list.selectedUID = parent.UID
	m.list.selectedProjectID = parent.ProjectID
	m.detail = detailModel{
		issue:    &child,
		scopePID: 7,
		navStack: []detailModel{{issue: &parent, scopePID: 7}},
	}
	m.input = newPanelPrompt(inputLabelPrompt, formTarget{projectID: 7, issueShortID: child.ShortID})

	after, cmd := m.cancelInput()

	require.Equal(t, child.UID, after.detail.issue.UID)
	require.Len(t, after.detail.navStack, 1, "dismissing a prompt must preserve back navigation")
	require.Nil(t, cmd)
}

func TestSplit_NewIssueSuccessReconcilesDeferredSelection(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = closing.ProjectID
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.input = newNewIssueForm()
	m.input.formGen = 9

	closed := closing
	closed.Status = "closed"
	m, _ = updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed, remaining},
	})
	require.True(t, m.splitDetailFollowPending)

	created := Issue{ProjectID: 7, UID: "01TEST-created", ShortID: "n004", Title: "new issue", Status: "open"}
	got, cmd := m.routeFormMutation(mutationDoneMsg{
		origin: "form", kind: "create", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &created},
	})
	after, ok := got.(Model)
	require.True(t, ok)

	require.Equal(t, inputNone, after.input.kind)
	require.Equal(t, remaining.UID, after.detail.issue.UID)
	require.False(t, after.splitDetailFollowPending)
	require.NotNil(t, cmd, "closing the form must return the deferred detail follow command")
}

func TestSplit_UndoEvidenceSuccessReconcilesDeferredSelection(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = closing.ProjectID
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.undoHistory.push(undoEntry{kind: "reopen", uid: closing.UID, projectID: 7, actor: "tester",
		before: Issue{Status: "closed"}, after: closing})
	m = m.openUndoCloseForm()
	entryID, formGen := m.undoHistory.entries[0].id, m.input.formGen

	closed := closing
	closed.Status = "closed"
	m, _ = updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed, remaining},
	})
	require.True(t, m.splitDetailFollowPending)

	after, cmd := updateModel(m, undoDoneMsg{
		entryID: entryID,
		formGen: formGen,
		outcome: undoOutcome{changed: true},
	})

	require.Equal(t, inputNone, after.input.kind)
	require.Equal(t, remaining.UID, after.detail.issue.UID)
	require.False(t, after.splitDetailFollowPending)
	require.NotNil(t, cmd, "undo completion must batch the deferred detail follow command")
}

func TestSplit_SearchQueryRefetchKeepsPriorDetail(t *testing.T) {
	m, cleanup := splitSearchTransitionFixture(t)
	defer cleanup()
	prior := *m.detail.issue

	after, cmd := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues: []Issue{
			{ProjectID: 7, UID: "01TEST-aaa1", ShortID: "aaa1", Title: "other row", Status: "open"},
		},
	})
	require.Equal(t, prior.UID, after.detail.issue.UID)
	require.Nil(t, cmd)
}

func TestSplit_ReorderRefetchKeepsSelectedDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	first := Issue{ProjectID: 7, UID: "01TEST-first", ShortID: "f001", Title: "first issue", Status: "open"}
	selected := Issue{ProjectID: 7, UID: "01TEST-selected", ShortID: "s002", Title: "selected issue", Status: "open"}
	m.list.issues = []Issue{first, selected}
	m.list.cursor = 1
	m.list.selectedUID = selected.UID
	m.list.selectedProjectID = 7
	m.detail.issue = &selected
	m.detail.gen = 41

	after, cmd := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{selected, first},
	})
	require.Equal(t, selected.UID, after.list.selectedUID)
	require.Equal(t, 0, after.list.cursor)
	require.Equal(t, selected.UID, after.detail.issue.UID)
	require.Equal(t, int64(41), after.detail.gen)
	require.Nil(t, cmd)
}

func TestSplit_RefetchKeepsPromptTargetWhileEditing(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = 7
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m.detail.gen = 41
	m.nextGen = 41
	m.input = newPanelPrompt(inputLabelPrompt, formTarget{projectID: 7, issueShortID: closing.ShortID})

	closed := closing
	closed.Status = "closed"
	after, cmd := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed, remaining},
	})
	require.Equal(t, remaining.UID, after.list.selectedUID)
	require.Equal(t, closing.UID, after.detail.issue.UID)
	require.Equal(t, int64(41), after.detail.gen)
	require.Nil(t, cmd)
}

func TestSplit_DismissPromptFollowsSelectionChangedDuringRefetch(t *testing.T) {
	for _, dismiss := range []struct {
		name string
		fn   func(Model) (Model, tea.Cmd)
	}{
		{name: "cancel", fn: Model.cancelInput},
		{name: "commit empty", fn: Model.commitInput},
	} {
		t.Run(dismiss.name, func(t *testing.T) {
			m, cleanup := splitTestSetup(t)
			defer cleanup()
			closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
			remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
			m.list.issues = []Issue{closing, remaining}
			m.list.filter = ListFilter{Status: "open"}
			m.list.selectedUID = closing.UID
			m.list.selectedProjectID = 7
			m.detail.issue = &closing
			m.input = newPanelPrompt(inputLabelPrompt, formTarget{projectID: 7, issueShortID: closing.ShortID})

			closed := closing
			closed.Status = "closed"
			m, _ = updateModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: []Issue{closed, remaining}})
			require.Equal(t, closing.UID, m.detail.issue.UID)

			after, cmd := dismiss.fn(m)
			require.Equal(t, inputNone, after.input.kind)
			require.Equal(t, remaining.UID, after.detail.issue.UID)
			require.NotNil(t, cmd)
		})
	}
}

func TestSplit_FormSuccessFollowsSelectionChangedDuringRefetch(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = 7
	m.detail.issue = &closing
	m.detail.scopePID = 7
	m = m.openBodyEditForm()
	m.input.saving = true

	closed := closing
	closed.Status = "closed"
	m, _ = updateModel(m, refetchedMsg{dispatchKey: m.currentCacheKey(), issues: []Issue{closed, remaining}})
	require.Equal(t, closing.UID, m.detail.issue.UID)

	after, _ := updateModel(m, mutationDoneMsg{
		origin: "form", kind: "form.body.edit", formGen: m.input.formGen,
		resp: &MutationResp{Issue: &closed},
	})
	require.Equal(t, inputNone, after.input.kind)
	require.Equal(t, remaining.UID, after.detail.issue.UID)
}

func TestSplit_RefetchKeepsAlreadyMatchingDetail(t *testing.T) {
	m, cleanup := splitTestSetup(t)
	defer cleanup()
	closing := Issue{ProjectID: 7, UID: "01TEST-closing", ShortID: "c002", Title: "closing issue", Status: "open"}
	remaining := Issue{ProjectID: 7, UID: "01TEST-remaining", ShortID: "r003", Title: "remaining issue", Status: "open"}
	m.list.issues = []Issue{closing, remaining}
	m.list.filter = ListFilter{Status: "open"}
	m.list.selectedUID = closing.UID
	m.list.selectedProjectID = 7
	m.detail.issue = &remaining
	m.detail.scopePID = 7
	m.detail.gen = 41
	m.nextGen = 41

	closed := closing
	closed.Status = "closed"
	after, cmd := updateModel(m, refetchedMsg{
		dispatchKey: m.currentCacheKey(),
		issues:      []Issue{closed, remaining},
	})
	require.Equal(t, remaining.UID, after.list.selectedUID)
	require.Equal(t, remaining.UID, after.detail.issue.UID)
	require.Equal(t, int64(41), after.detail.gen)
	require.Nil(t, cmd)
}
