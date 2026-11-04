package tui

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

// Contract: a real run issue link opens the issue synchronously and back returns
// to the exact Cron frame, even when the hidden split list selects another issue.
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
	m = sendKey(m, tea.KeyEnter)
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
			require.Equal(t, "issue-example", m.detail.issue.UID)
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
