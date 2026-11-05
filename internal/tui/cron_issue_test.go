package tui

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
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

func TestSnapshot_CronIssue(t *testing.T) {
	defer snapshotInit(t)()
	m := cronIssueFixture(t, 100)
	assertGolden(t, "cron-run-detail-issue", m.viewBody())
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
	require.Equal(t, "Moved issue", m.detail.issue.Title)
	require.Equal(t, "wxyz", m.detail.issue.ShortID)
	require.False(t, m.closeRequiresEvidence)
	m, cmd = updateModel(m, keyRune('x'))
	require.NotNil(t, cmd)
	cmd()
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"/api/v1/projects/9/issues/wxyz/actions/close"}, closes, "the close must reach the moved issue, not the source project's colliding short ID")
}

// An invalidation during a moved issue's refresh must use its accepted project
// and short ID together, even when the issue response finishes the batch.
func TestCronIssueMovedIssueQueuedInvalidation(t *testing.T) {
	m := cronIssueFixture(t, 80)
	var mu sync.Mutex
	var paths []string
	m.api = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		uid, pid, ref := "issue-example", 7, "abcd"
		switch r.URL.Path {
		case "/api/v1/projects/7/issues/issue-example":
		case "/api/v1/projects/7/issues/abcd", "/api/v1/projects/9/issues/wxyz":
			pid, ref = 9, "wxyz"
		case "/api/v1/projects/7/issues/wxyz":
			uid, ref = "colliding-issue", "wxyz"
		default:
			http.Error(w, "fixture not found", http.StatusNotFound)
			return
		}
		respondJSON(t, w, map[string]any{"issue": map[string]any{"uid": uid, "project_id": pid, "short_id": ref, "title": "Linked issue", "status": "open"}})
	})
	m, cmd := m.cronOpenIssue("issue-example")
	defer func() { m.detail.cancelReads() }()
	for _, msg := range runCmdTree(cmd) {
		m, _ = updateModel(m, msg)
	}
	require.Equal(t, int64(7), m.detail.scopePID)
	event := eventReceivedMsg{eventType: "issue.updated", projectID: 7, issueUID: "issue-example"}
	tick := m.maybeRefetchOpenDetail(event)
	require.NotNil(t, tick)
	m, cmd = updateModel(m, tick())
	results := runCmdTree(cmd)
	m, _ = updateModel(m, event)
	require.True(t, m.detail.reads.dirty, "the next invalidation waits for the active batch")
	var issue detailFetchedMsg
	for _, msg := range results {
		if fetched, ok := msg.(detailFetchedMsg); ok {
			issue = fetched
		} else {
			m, _ = updateModel(m, msg)
		}
	}
	require.NotNil(t, issue.issue)
	require.Equal(t, int64(9), issue.issue.ProjectID)
	require.Equal(t, "wxyz", issue.issue.ShortID)
	require.Equal(t, uint8(1), m.detail.reads.remaining, "only the moved issue response remains")
	mu.Lock()
	paths = nil
	mu.Unlock()
	m, cmd = updateModel(m, issue)
	require.NotNil(t, cmd, "the queued invalidation starts a trailing refresh")
	for _, msg := range runCmdTree(cmd) {
		m, _ = updateModel(m, msg)
	}
	mu.Lock()
	defer mu.Unlock()
	require.ElementsMatch(t, []string{
		"/api/v1/projects/9/issues/wxyz",
		"/api/v1/projects/9/issues/wxyz",
		"/api/v1/projects/9/events",
		"/api/v1/projects/9/issues/wxyz",
	}, paths, "all trailing reads must follow the moved issue to its destination")
	require.NoError(t, m.detail.err)
	require.Equal(t, "issue-example", m.detail.issue.UID, "a source-project short-ID collision must not replace the linked issue")
}

// Before the linked issue's read returns, the detail has no identity: actions
// that need one, such as a new child, wait instead of using an empty parent.
func TestCronIssueActionsWaitForIssue(t *testing.T) {
	for _, state := range []string{"loading", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			m := cronIssueFixture(t, 80)
			m, _ = m.cronOpenIssue("issue-example")
			require.True(t, m.detail.loading)
			require.Equal(t, "issue-example", m.detail.readRef)
			if state == "unreadable" {
				m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("issue not found")})
			}
			m, cmd := updateModel(m, keyRune('N'))
			require.False(t, slices.ContainsFunc(runCmdTree(cmd), func(msg tea.Msg) bool { _, ok := msg.(openInputMsg); return ok }), "N without issue identity must not open a parentless form")
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
		})
	}
}

// Linked-issue reads show their pending/error state in both layouts. Esc
// returns to Cron, and failed reads cannot launch identity-dependent actions.
func TestCronIssueSplitPaneShowsReadState(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := cronIssueFixture(t, width)
			m = sendKey(m, tea.KeyEnter)
			require.True(t, m.cron.returnToCron)
			if width == 80 {
				require.Equal(t, splitlayout.Stacked, m.layout)
			} else {
				require.Equal(t, splitlayout.Split, m.layout)
				require.True(t, m.detail.loading)
				require.Equal(t, "issue-example", m.detail.readRef)
				require.Nil(t, m.detail.issue)
				view := ansi.Strip(m.viewBody())
				require.Contains(t, view, "loading…")
				require.NotContains(t, view, "select an issue from the list pane")
			}
			m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("issue not found")})
			require.Nil(t, m.detail.issue)
			require.ErrorContains(t, m.detail.err, "issue not found")
			require.True(t, m.cron.returnToCron)
			require.Nil(t, m.toast, "the visible error already explains; a notice would repeat it")
			view := ansi.Strip(m.viewBody())
			require.Contains(t, view, "issue not found")
			require.Contains(t, view, "esc")
			require.Contains(t, view, "back")
			if width == 80 {
				require.NotContains(t, view, "no issue selected")
			} else {
				require.Contains(t, view, "unavailable: issue not found")
				require.NotContains(t, view, "select an issue from the list pane")
				var cmd tea.Cmd
				m, cmd = updateModel(m, keyRune('N'))
				require.False(t, slices.ContainsFunc(runCmdTree(cmd), func(msg tea.Msg) bool { _, ok := msg.(openInputMsg); return ok }))
				require.Equal(t, inputNone, m.input.kind)
			}
			stacked := m
			m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			m = drainInboxCmds(m, cmd)
			require.Equal(t, viewCron, m.view)
			if width == 80 {
				stacked, cmd = updateModel(stacked, popDetailMsg{})
				stacked = drainInboxCmds(stacked, cmd)
				require.Equal(t, viewCron, stacked.view)
			}
		})
	}
}

// A failed linked-issue read gets a notice only where no persistent error is
// on screen: behind Help or the narrow hint, or on a retained issue's refresh.
func TestCronIssueReadFailureNoticeWhenHidden(t *testing.T) {
	for _, mode := range []string{"behind help", "behind narrow", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			m := cronIssueFixture(t, 80)
			m = sendKey(m, tea.KeyEnter)
			require.True(t, m.cron.returnToCron)
			switch mode {
			case "behind help":
				m = sendRune(m, '?')
				require.Equal(t, viewHelp, m.view)
			case "behind narrow":
				m = resizeModel(m, 60, 40)
				require.Contains(t, ansi.Strip(m.View().Content), "needs more space")
			case "refresh":
				m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, issue: &Issue{UID: "issue-example", ShortID: "abcd", Title: "Linked result", ProjectID: 7}})
				require.Nil(t, m.toast)
			}
			if mode == "refresh" {
				api := &fakeDetailAPI{getIssueErr: errors.New("server unavailable")}
				cmd := m.detail.refetch(api)
				require.NotNil(t, cmd)
				defer func() { m.detail.cancelReads() }()
				fetched := 0
				for _, msg := range runCmdTree(cmd) {
					if result, ok := msg.(detailFetchedMsg); ok {
						fetched++
						require.EqualError(t, result.err, "server unavailable")
					}
					m, _ = updateModel(m, msg)
				}
				require.Equal(t, 1, fetched)
				require.Equal(t, "abcd", api.lastGetIssue)
			} else {
				m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("server unavailable")})
			}
			require.True(t, m.cron.returnToCron)
			require.NotNil(t, m.toast, "no persistent error is visible, so a notice explains the failure")
			require.Contains(t, m.toast.text, "server unavailable")
			if mode == "refresh" {
				require.Equal(t, "Linked result", m.detail.issue.Title, "a failed refresh keeps the retained issue")
			}
		})
	}
}

// A layout change while a linked issue loads, or after its read failed, keeps
// that issue in front, also behind Help, and its return to the Cron frame.
func TestCronIssueLayoutChangeKeepsReturn(t *testing.T) {
	for _, from := range []int{80, 160} {
		for _, mode := range []string{"loading", "failed", "help"} {
			t.Run(fmt.Sprintf("%d/%s", from, mode), func(t *testing.T) {
				m := cronIssueFixture(t, from)
				stack := slices.Clone(m.cron.stack)
				m = sendKey(m, tea.KeyEnter)
				if mode == "failed" {
					m, _ = updateModel(m, detailFetchedMsg{gen: m.detail.gen, err: errors.New("issue not found")})
				}
				if mode == "help" {
					m = sendRune(m, '?')
				}
				m = resizeModel(m, 240-from, 40)
				if mode == "help" {
					m = sendRune(m, '?')
				}
				require.Equal(t, viewDetail, m.view)
				require.Equal(t, focusDetail, m.focus)
				require.True(t, m.cron.returnToCron)
				m, cmd := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
				m = drainInboxCmds(m, cmd)
				require.Equal(t, viewCron, m.view)
				require.Equal(t, stack, m.cron.stack)
			})
		}
	}
}
