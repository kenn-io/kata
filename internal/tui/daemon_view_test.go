package tui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDaemonView_DKeyTransitionsFromList(t *testing.T) {
	m := setupDaemonViewSource()

	out, cmd := updateModel(m, keyRune('D'))

	require.Nil(t, cmd)
	assert.Equal(t, viewDaemons, out.view)
	assert.Equal(t, viewList, out.prevView)
	assert.Equal(t, 1, out.daemonCursor, "cursor should land on the active daemon")
}

func TestDaemonView_DKeyTransitionsFromEmpty(t *testing.T) {
	m := setupDaemonViewSource()
	m.view = viewEmpty

	out, cmd := updateModel(m, keyRune('D'))

	require.Nil(t, cmd)
	assert.Equal(t, viewDaemons, out.view)
	assert.Equal(t, viewEmpty, out.prevView)
}

func TestDaemonTargetsMatchImplicitLocalToNamedLocal(t *testing.T) {
	assert.True(t,
		daemonTargetsMatch(daemonTarget{Name: "local", Local: true}, daemonTarget{Local: true}),
		"implicit active local daemon should match a named local catalog row")
}

func TestDaemonView_EscReturnsToPreviousView(t *testing.T) {
	m := setupDaemonViewSource()
	m.view = viewDetail
	m.detail = detailModel{issue: &Issue{ShortID: "abc1"}}

	out, _ := updateModel(m, keyRune('D'))
	out, cmd := out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyEsc})

	require.Nil(t, cmd)
	assert.Equal(t, viewDetail, out.view)
	assert.Equal(t, "abc1", out.detail.issue.ShortID)
}

func TestDaemonView_CursorMovement(t *testing.T) {
	m := setupDaemonView()

	out, _ := m.routeDaemonsViewKey(keyRune('j'))
	assert.Equal(t, 1, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(keyRune('j'))
	assert.Equal(t, 2, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(keyRune('k'))
	assert.Equal(t, 1, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyHome})
	assert.Equal(t, 0, out.daemonCursor)
	out, _ = out.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyEnd})
	assert.Equal(t, 2, out.daemonCursor)
}

func TestDaemonView_RenderIncludesDaemonRows(t *testing.T) {
	m := setupDaemonView()

	out := stripANSI(renderDaemons(m))

	assertContains(t, out, "kata / daemons", "missing daemon view title")
	assertContains(t, out, "local", "missing local daemon")
	assertContains(t, out, "shared", "missing shared daemon")
	assertContains(t, out, "daemon.internal:7777", "missing endpoint host")
	assertContains(t, out, "token", "missing token indicator")
	assertContains(t, out, "current", "missing current marker")
}

func TestDaemonView_RenderKeepsConfiguredTextSingleLine(t *testing.T) {
	row := daemonRow{
		target: daemonTarget{
			Name: "shared\nname",
			URL:  "https://daemon.example:7777\textra",
		},
		current: true,
	}

	rendered := stripANSI(renderDaemonRow(row, false, 100))
	footer := stripANSI(daemonFooter(row, 100))

	assert.NotContains(t, rendered, "\n")
	assert.NotContains(t, footer, "\n")
	assertContains(t, rendered, `shared\nname`, "row name must be line-sanitized")
	assertContains(t, rendered, "https://daemon.example:7777 extra", "row endpoint must be line-sanitized")
	assertContains(t, footer, `shared\nname`, "footer name must be line-sanitized")
	assertContains(t, footer, "https://daemon.example:7777 extra", "footer endpoint must be line-sanitized")
}

func TestDaemonView_HelpIncludesDaemonBinding(t *testing.T) {
	out := stripANSI(renderHelp(newKeymap(), 100, ListFilter{}))

	assertContains(t, out, "D", "help overlay missing daemon binding")
	assertContains(t, out, "daemons", "help overlay missing daemon description")
}

func TestDaemonView_EnterDispatchesSwitchCommand(t *testing.T) {
	oldConnect := connectDaemonTargetForTUI
	t.Cleanup(func() { connectDaemonTargetForTUI = oldConnect })
	connectDaemonTargetForTUI = func(_ context.Context, target daemonTarget) (daemonConnection, error) {
		return daemonConnection{
			api:      &Client{},
			endpoint: target.URL,
			target:   target,
			init:     bootInit{view: viewEmpty, scope: scope{empty: true}},
		}, nil
	}
	m := setupDaemonView()
	m.daemonCursor = 2

	out, cmd := m.routeDaemonsViewKey(tea.KeyMsg{Type: tea.KeyEnter})

	require.NotNil(t, cmd)
	assert.Equal(t, viewDaemons, out.view, "view should remain until connection succeeds")
	msg := cmd()
	sw, ok := msg.(daemonSwitchResultMsg)
	require.True(t, ok)
	require.NoError(t, sw.err)
	assert.Equal(t, "prod", sw.conn.target.Name)
	assert.Equal(t, uint64(1), sw.attempt)
	assert.Equal(t, uint64(1), out.daemonSwitchAttempt)
}

func TestDaemonSwitchDropsOutOfOrderAttempt(t *testing.T) {
	m := setupDaemonViewSource()
	m.connGen = 2
	m.daemonSwitchAttempt = 2
	m.activeDaemon = daemonTarget{Name: "current", URL: "https://current.example"}
	conn := daemonConnection{
		api:    &Client{},
		target: daemonTarget{Name: "older", URL: "https://older.example"},
		init:   bootInit{view: viewList, scope: homedScope(9, "older")},
	}

	out, cmd := updateModel(m, daemonSwitchResultMsg{attempt: 1, conn: conn})

	assert.Equal(t, uint64(2), out.connGen)
	assert.Equal(t, "current", out.activeDaemon.Name)
	assert.Nil(t, cmd)
}

func TestDaemonSwitchSuccessResetsDaemonLocalState(t *testing.T) {
	restarted := false
	m := setupDaemonViewSource()
	m.connGen = 4
	m.api = &Client{}
	m.scope = homedScope(7, "old")
	m.layout = layoutSplit
	m.focus = focusDetail
	m.view = viewDetail
	m.list = newListModel()
	m.list.actor = "tester"
	m.list.issues = []Issue{testIssue("old1")}
	m.detail = detailModel{issue: &Issue{ProjectID: 7, ShortID: "old1"}}
	m.cache.put(cacheKey{projectID: 7, limit: queueFetchLimit}, []Issue{testIssue("old1")})
	m.projectLabels = newLabelCache()
	m.projectLabels.byProject[7] = labelCacheEntry{labels: []LabelCount{{Label: "old", Count: 1}}}
	m.projectsByID[7] = "old"
	m.projectStats[7] = ProjectStatsSummary{Open: 1}
	m.pendingRefetch = true
	m.projectsStale = true
	m.projectsRefetchPending = true
	m.input = newSearchBar(ListFilter{})
	m.modal = modalQuitConfirm
	m.sseRestart = func(daemonConnection, uint64, chan tea.Msg) tea.Cmd {
		return func() tea.Msg {
			restarted = true
			return nil
		}
	}
	conn := daemonConnection{
		api:      &Client{},
		sseHC:    &http.Client{},
		endpoint: "https://new.example",
		target:   daemonTarget{Name: "new", URL: "https://new.example"},
		catalog:  m.daemonTargets,
		init: bootInit{
			view:  viewList,
			scope: homedScope(9, "new-project"),
		},
	}

	out, cmd := updateModel(m, daemonSwitchResultMsg{conn: conn})

	assert.Equal(t, uint64(5), out.connGen)
	assert.Equal(t, "new", out.activeDaemon.Name)
	assert.Equal(t, int64(9), out.scope.projectID)
	assert.Equal(t, viewList, out.view)
	assert.Equal(t, focusList, out.focus)
	assert.Equal(t, "tester", out.list.actor)
	assert.Empty(t, out.list.issues)
	assert.Nil(t, out.detail.issue)
	assert.False(t, out.cache.isStale())
	assert.Empty(t, out.projectLabels.byProject)
	assert.Empty(t, out.projectsByID)
	assert.False(t, out.pendingRefetch)
	assert.False(t, out.projectsStale)
	assert.False(t, out.projectsRefetchPending)
	assert.Equal(t, inputNone, out.input.kind)
	assert.Equal(t, modalNone, out.modal)
	require.NotNil(t, cmd)
	runBatchCmd(cmd)
	assert.True(t, restarted)
}

func TestDaemonSwitchFailureKeepsCurrentSession(t *testing.T) {
	m := setupDaemonViewSource()
	m.connGen = 2
	m.scope = homedScope(7, "old")
	m.cache.put(cacheKey{projectID: 7, limit: queueFetchLimit}, []Issue{testIssue("old1")})
	active := m.activeDaemon

	out, cmd := updateModel(m, daemonSwitchResultMsg{
		target: daemonTarget{Name: "broken", URL: "https://broken.example"},
		err:    assert.AnError,
	})

	assert.Equal(t, uint64(2), out.connGen)
	assert.Equal(t, active, out.activeDaemon)
	assert.Equal(t, int64(7), out.scope.projectID)
	assert.False(t, out.cache.isStale())
	require.NotNil(t, out.toast)
	assert.Contains(t, out.toast.text, "broken")
	require.NotNil(t, cmd)
}

func TestDaemonSwitchFailureSanitizesToast(t *testing.T) {
	m := setupDaemonViewSource()

	out, _ := updateModel(m, daemonSwitchResultMsg{
		target: daemonTarget{Name: "bad\x1b]0;owned\a"},
		err:    errors.New("boom\x1b[31mred\x1b[0m\nnext"),
	})

	require.NotNil(t, out.toast)
	assert.NotContains(t, out.toast.text, "\x1b")
	assert.NotContains(t, out.toast.text, "\a")
	assert.False(t, strings.Contains(out.toast.text, "\n"), "toast must stay single-line")
	assert.Contains(t, out.toast.text, `\n`)
}

func TestDaemonSwitchDropsOldSSEMessages(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.cache.put(cacheKey{projectID: 7, limit: queueFetchLimit}, []Issue{testIssue("old1")})

	out, cmd := updateModel(m, eventReceivedMsg{gen: 1, eventType: "issue.created", projectID: 7})

	assert.False(t, out.cache.isStale())
	assert.False(t, out.pendingRefetch)
	assert.NotNil(t, cmd, "dropping stale SSE must still re-arm the SSE bridge")
}

func TestDaemonSwitchDropsOldListFetch(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	keep := []Issue{testIssue("keep")}
	m.list.issues = keep
	m.cache.put(cacheKey{projectID: 7, limit: queueFetchLimit}, keep)

	out, cmd := updateModel(m, refetchedMsg{
		connGen:     1,
		dispatchKey: cacheKey{projectID: 7, limit: queueFetchLimit},
		issues:      []Issue{testIssue("stale")},
	})

	assert.Nil(t, cmd)
	assert.Equal(t, keep, out.list.issues)
}

func TestDaemonSwitchDropsOldListMutation(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.view = viewList
	m.list.status = "keep"

	out, cmd := updateModel(m, mutationDoneMsg{
		connGen: 1,
		origin:  "list",
		kind:    "close",
		err:     assert.AnError,
	})

	assert.Nil(t, cmd)
	assert.Equal(t, "keep", out.list.status)
}

func TestDaemonSwitchDropsOldOpenDetail(t *testing.T) {
	m := newTestModel()
	m.connGen = 2

	out, cmd := updateModel(m, openDetailMsg{
		connGen: 1,
		issue:   Issue{ProjectID: 7, UID: "old-uid", ShortID: "old1", Title: "old"},
	})

	assert.Nil(t, cmd)
	assert.Equal(t, viewList, out.view)
	assert.Nil(t, out.detail.issue)
}

func TestDaemonSwitchDropsOldJumpDetail(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.view = viewDetail
	m.detail.issue = &Issue{ProjectID: 7, UID: "new-uid", ShortID: "new1", Title: "new"}
	m.detail.scopePID = 7
	m.detail.gen = 4

	out, cmd := updateModel(m, jumpDetailMsg{connGen: 1, ref: "old1"})

	assert.Nil(t, cmd)
	assert.Equal(t, viewDetail, out.view)
	require.NotNil(t, out.detail.issue)
	assert.Equal(t, "new1", out.detail.issue.ShortID)
	assert.Equal(t, int64(4), out.detail.gen)
}

func TestDaemonSwitchDropsOldPopDetail(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.view = viewDetail
	m.focus = focusDetail
	m.detail.issue = &Issue{ProjectID: 7, UID: "new-uid", ShortID: "new1", Title: "new"}

	out, cmd := updateModel(m, popDetailMsg{connGen: 1})

	assert.Nil(t, cmd)
	assert.Equal(t, viewDetail, out.view)
	assert.Equal(t, focusDetail, out.focus)
}

func TestDaemonSwitchDropsOldProjectsFetch(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.projectsGen = 3
	m.projectsByID = map[int64]string{7: "keep"}

	out, cmd := updateModel(m, projectsLoadedMsg{
		connGen:  1,
		gen:      3,
		projects: map[int64]string{7: "stale"},
	})

	assert.Nil(t, cmd)
	assert.Equal(t, map[int64]string{7: "keep"}, out.projectsByID)
}

func TestDaemonSwitchResetsProjectsGeneration(t *testing.T) {
	m := newTestModel()
	m.connGen = 2
	m.projectsGen = 5
	m.projectsByID = map[int64]string{7: "old"}
	conn := daemonConnection{
		api:    &Client{},
		target: daemonTarget{Name: "new", URL: "https://new.example"},
		init: bootInit{
			view:  viewList,
			scope: homedScope(9, "new-project"),
		},
	}

	out, _ := updateModel(m, daemonSwitchResultMsg{conn: conn})

	assert.Equal(t, uint64(0), out.projectsGen)
}

func setupDaemonViewSource() Model {
	m := initialModel(Options{})
	m.view = viewList
	m.width, m.height = 120, 24
	m.activeDaemon = daemonTarget{Name: "shared", URL: "http://daemon.internal:7777"}
	m.daemonTargets = []daemonTarget{
		{Name: "local", Local: true},
		{Name: "shared", URL: "http://daemon.internal:7777", Token: "tok", AllowInsecure: true},
		{Name: "prod", URL: "https://kata.example.test"},
	}
	return m
}

func setupDaemonView() Model {
	m := setupDaemonViewSource()
	m.view = viewDaemons
	return m
}

func runBatchCmd(cmd tea.Cmd) {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		if len(batch) > 0 && batch[0] != nil {
			_ = batch[0]()
		}
	}
}
