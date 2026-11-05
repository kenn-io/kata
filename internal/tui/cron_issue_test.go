package tui

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

func cronIssueFixture(t *testing.T, width int) Model {
	t.Helper()
	m := cronFixture(t)
	m = resizeModel(m, width, 40)
	m = sendRune(m, '3')
	m.cron.data.runs[0].IssueUID = new("issue-example")
	m.list.actor = "operator"
	m.list.issues = []Issue{testIssue("hidden-list")}
	m.list.loading = false
	original := testIssue("origin-example")
	m.detail.issue = &original
	m = cronSettleTest(sendKey(m, tea.KeyEnter))
	_, _, lines := cronScreen(m)
	for i, line := range lines {
		if line.target.kind == cronIssue {
			m.cron.stack[len(m.cron.stack)-1].cursor = i
			return m
		}
	}
	t.Fatal("run detail has no followable issue link")
	return m
}

// Contract: a real run issue link opens the issue synchronously and back returns
// to the exact Cron frame, even when the hidden split list selects another issue.
func TestCronIssueRoundTrip(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := cronIssueFixture(t, width)
			// cron.lists is a [4]cronList value array (int/string fields), so this is an independent copy.
			stack, lists := slices.Clone(m.cron.stack), m.cron.lists
			previousGen := m.nextGen
			previousView := m.prevView
			originalUID := m.detail.issue.UID
			m = sendKey(m, tea.KeyEnter)
			require.Equal(t, viewDetail, m.view)
			require.True(t, m.cron.returnToCron)
			m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, issue: &Issue{UID: "issue-example", ShortID: "abcd", ProjectID: 7}})
			require.Equal(t, int64(7), m.detail.scopePID)
			require.Equal(t, "operator", m.detail.actor)
			require.Greater(t, m.detail.gen, previousGen)
			// Every list-follow path, including an already pending debounce, must yield.
			m.pendingDetailFollow = true
			m, _ = updateModel(m, detailFollowTickMsg{gen: m.nextDetailFollowGen})
			m, _ = m.followHighlightedIssueIfNeeded(nil)
			m, _ = m.reconcileSplitDetailAfterRefetch(7, "old-list", true)
			require.Equal(t, "issue-example", m.detail.issue.UID)
			m = sendRune(m, '?')
			m = sendRune(m, '?')
			require.True(t, m.cron.returnToCron)
			// Nested issue back remains ordinary detail navigation before returning.
			first := m.detail
			child := testIssue("child-example")
			m.detail.issue = &child
			m.detail.navStack = []detailModel{first}
			m = sendKey(m, tea.KeyEscape)
			require.Equal(t, "issue-example", m.detail.issue.UID)
			require.Equal(t, viewDetail, m.view)
			var cmd tea.Cmd
			m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			m = drainInboxCmds(m, cmd)
			require.Equal(t, viewCron, m.view)
			require.False(t, m.cron.returnToCron)
			require.Equal(t, stack, m.cron.stack)
			require.Equal(t, lists, m.cron.lists)
			require.Equal(t, previousView, m.prevView)
			require.Equal(t, originalUID, m.detail.issue.UID)
		})
	}
}

func TestCronIssueReturnAuthorityClears(t *testing.T) {
	for _, navigation := range []string{"scope", "connection", "projects", "split-list", "cron-tab"} {
		t.Run(navigation, func(t *testing.T) {
			m := cronIssueFixture(t, 160)
			m = sendKey(m, tea.KeyEnter)
			require.Equal(t, splitlayout.Split, m.layout)
			switch navigation {
			case "scope":
				m.scopeGen++
				m, _ = updateModel(m, tea.FocusMsg{})
			case "connection":
				m.connGen++
				m, _ = updateModel(m, tea.FocusMsg{})
			case "projects":
				m = sendRune(m, 'P')
			case "split-list":
				m = sendKey(m, tea.KeyTab)
			case "cron-tab":
				m = sendRune(m, '2')
			}
			require.False(t, m.cron.returnToCron)
		})
	}
}

func TestCronIssueUnreadableStillReturns(t *testing.T) {
	m := cronIssueFixture(t, 80)
	m = sendKey(m, tea.KeyEnter)
	// The ordinary show-error path leaves a readable error and the Cron return.
	m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("issue not found")})
	require.ErrorContains(t, m.detail.err, "issue not found")
	require.True(t, m.cron.returnToCron, "the unreadable link keeps the Cron return route")
	require.NotNil(t, m.toast)
	require.Contains(t, m.toast.text, "issue not found")
	m, cmd := updateModel(m, popDetailMsg{})
	m = drainInboxCmds(m, cmd)
	require.Equal(t, viewCron, m.view)
}

func TestSnapshot_CronIssue(t *testing.T) {
	defer snapshotInit(t)()
	m := cronIssueFixture(t, 100)
	assertGolden(t, "cron-run-detail-issue", m.viewBody())
}

func TestCronIssueFetchByUID(t *testing.T) {
	m := cronIssueFixture(t, 80)
	var fetched atomic.Bool
	m.api = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/7/issues/issue-example" {
			fetched.Store(true)
			respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": "issue-example", "project_id": 7, "short_id": "abcd", "title": "Linked issue", "status": "open"}})
			return
		}
		http.Error(w, "fixture not found", http.StatusNotFound)
	})
	m, cmd := m.cronOpenIssue("issue-example")
	require.NotNil(t, cmd)
	for _, msg := range runCmdTree(cmd) {
		if _, ok := msg.(detailFetchedMsg); ok {
			m, _ = updateModel(m, msg)
		}
	}
	require.True(t, fetched.Load(), "opening a UID link must fetch the issue, even before its short ID is known")
	require.Equal(t, "Linked issue", m.detail.issue.Title)
}

// A linked issue moved after the run named it. The UID read resolves through
// the source project's history; later short-ID actions must follow the issue
// to its destination, even when the source reuses that short ID.
func TestCronIssueMovedIssueTargetsDestination(t *testing.T) {
	m := cronIssueFixture(t, 80)
	var mu sync.Mutex
	var closes []string
	m.api = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects/7/issues/issue-example":
			respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": "issue-example", "project_id": 9, "short_id": "wxyz", "title": "Moved issue", "status": "open"}})
		case "/api/v1/projects/7/issues/wxyz/actions/close", "/api/v1/projects/9/issues/wxyz/actions/close":
			mu.Lock()
			closes = append(closes, r.URL.Path)
			mu.Unlock()
			uid, pid := "issue-example", 9
			if strings.Contains(r.URL.Path, "/projects/7/") {
				uid, pid = "colliding-issue", 7
			}
			respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": uid, "project_id": pid, "short_id": "wxyz", "status": "closed"}, "changed": true})
		default:
			http.Error(w, "fixture not found", http.StatusNotFound)
		}
	})
	m, cmd := m.cronOpenIssue("issue-example")
	for _, msg := range runCmdTree(cmd) {
		m, _ = updateModel(m, msg)
	}
	require.Equal(t, "wxyz", m.detail.issue.ShortID)
	require.False(t, m.closeRequiresEvidence)
	m, cmd = updateModel(m, keyRune('x'))
	require.NotNil(t, cmd)
	cmd()
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/api/v1/projects/9/issues/wxyz/actions/close"}, closes, "the close must reach the moved issue, not the source project's colliding short ID")
}

// Before the linked issue's read returns, the detail has no identity: actions
// that need one, such as a new child, wait instead of using an empty parent.
func TestCronIssueActionsWaitForIssue(t *testing.T) {
	m := cronIssueFixture(t, 80)
	m, _ = m.cronOpenIssue("issue-example")
	require.True(t, m.detail.loading)
	require.Equal(t, "issue-example", m.detail.readRef)
	m, cmd := updateModel(m, keyRune('N'))
	require.False(t, slices.ContainsFunc(runCmdTree(cmd), func(msg tea.Msg) bool { _, ok := msg.(openInputMsg); return ok }), "N before the issue loads must not open a parentless form")
	require.Equal(t, inputNone, m.input.kind)
	m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, issue: &Issue{UID: "issue-example", ShortID: "abcd", ProjectID: 7}})
	m, cmd = updateModel(m, keyRune('N'))
	var opened *openInputMsg
	for _, msg := range runCmdTree(cmd) {
		if o, ok := msg.(openInputMsg); ok {
			opened = &o
		}
	}
	require.NotNil(t, opened)
	require.NotNil(t, opened.parentShortID)
	require.Equal(t, "abcd", *opened.parentShortID)
}

// Stacked detail has no pane frame around it, so a linked issue that failed to
// load must itself show why and how to return, while its actions stay inert.
func TestCronIssueUnreadableStackedView(t *testing.T) {
	m := cronIssueFixture(t, 80)
	m = sendKey(m, tea.KeyEnter)
	require.Equal(t, splitlayout.Stacked, m.layout)
	m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("issue not found")})
	require.Nil(t, m.detail.issue)
	m.toast = nil // the notice expires; the screen must still explain
	view := ansi.Strip(m.viewBody())
	require.Contains(t, view, "issue not found")
	require.Contains(t, view, "esc")
	require.Contains(t, view, "back")
	require.NotContains(t, view, "no issue selected")
	m, cmd := updateModel(m, keyRune('N'))
	require.False(t, slices.ContainsFunc(runCmdTree(cmd), func(msg tea.Msg) bool { _, ok := msg.(openInputMsg); return ok }), "N on an unreadable issue must not open a parentless form")
	require.Equal(t, inputNone, m.input.kind)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = drainInboxCmds(m, cmd)
	require.Equal(t, viewCron, m.view)
}
