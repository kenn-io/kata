package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/splitlayout"
)

// The builder contract adds wrapping navigation only where Tab has no owner.
func TestTopLevelTabCycling(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			m := sendRune(cronFixture(t), '1')
			key := tea.Key{Code: tea.KeyTab}
			want := []int{cronJobs, cronRuns, cronWorkflows, cronKata}
			if reverse {
				key.Mod = tea.ModShift
				want = []int{cronWorkflows, cronRuns, cronJobs, cronKata}
			}
			for _, tab := range want {
				m, _ = updateModel(m, tea.KeyPressMsg(key))
				if tab == cronKata {
					require.Equal(t, viewList, m.view)
				} else {
					require.Equal(t, viewCron, m.view)
					require.Equal(t, tab, m.cron.tab)
				}
			}
		})
	}
}

func TestTopLevelTabKeepsContextBindings(t *testing.T) {
	t.Run("split pane", func(t *testing.T) {
		m := sendRune(cronFixture(t), '1')
		m.layout, m.focus = splitlayout.Split, focusList
		iss := testIssue("abc1")
		m.detail.issue = &iss
		m = sendKey(m, tea.KeyTab)
		require.Equal(t, viewList, m.view)
		require.Equal(t, focusDetail, m.focus)
	})
	t.Run("detail activity", func(t *testing.T) {
		m := sendRune(cronFixture(t), '1')
		m.view = viewDetail
		iss := testIssue("abc1")
		m.detail.issue = &iss
		m.detail.comments = []CommentEntry{{Body: "A comment"}}
		before := m.detail.activeTab
		m = sendKey(m, tea.KeyTab)
		require.Equal(t, viewDetail, m.view)
		require.NotEqual(t, before, m.detail.activeTab)
	})
	t.Run("cron detail", func(t *testing.T) {
		m := sendKey(cronFixture(t), tea.KeyEnter)
		m = sendKey(m, tea.KeyTab)
		require.Equal(t, cronJobs, m.cron.tab)
		require.Len(t, m.cron.stack, 1)
	})
	t.Run("cron filter", func(t *testing.T) {
		m := sendRune(cronFixture(t), '/')
		m = sendKey(m, tea.KeyTab)
		require.True(t, m.cron.filtering)
		require.Equal(t, cronJobs, m.cron.tab)
	})
	t.Run("job editor", func(t *testing.T) {
		m := sendRune(cronFixture(t), 'n')
		require.NotNil(t, m.cron.form)
		before := m.cron.form.focus
		m = sendKey(m, tea.KeyTab)
		require.Equal(t, cronJobs, m.cron.tab)
		require.NotEqual(t, before, m.cron.form.focus)
	})
}

// Every top-level screen must keep navigation visible, including issue pages
// and auxiliary screens. This also catches losing the row to an auth banner.
func TestSharedTabHeaderEveryView(t *testing.T) {
	for _, view := range []viewID{viewList, viewDetail, viewHelp, viewEmpty, viewProjects, viewDaemons, viewFederation, viewCredentials, viewAttention, viewCron} {
		t.Run(fmt.Sprint(view), func(t *testing.T) {
			m := sendRune(cronFixture(t), '1')
			m.view, m.width = view, 140
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			iss := testIssue("abc1")
			m.detail.issue = &iss
			if view == viewCron {
				m.cron.tab = cronJobs
			}
			for _, unhealthy := range []bool{false, true} {
				m.sseStatus = sseConnected
				if unhealthy {
					m.sseStatus = sseReconnecting
				}
				body := stripANSI(m.viewContent())
				first, _, _ := strings.Cut(body, "\n")
				selected := "[kata] カタ jobs runs workflows"
				if view == viewCron {
					selected = "kata カタ [jobs] runs workflows"
				}
				require.Contains(t, first, selected)
				require.Contains(t, first, "Project: example-project")
				require.Contains(t, first, "Daemon: shared-hub")
				require.Equal(t, 1, strings.Count(body, selected))
				if unhealthy {
					require.Contains(t, strings.Split(body, "\n")[1], "Reconnecting")
				} else {
					require.NotContains(t, body, "Connected")
					require.NotContains(t, body, "Hub:")
				}
			}
		})
	}
}

func TestSelectedTabHasNoUnderline(t *testing.T) {
	t.Cleanup(applyDefaultColorMode)
	for _, mode := range []colorMode{colorNone, colorDark, colorLight} {
		applyColorMode(mode, false)
		require.False(t, tabActive.GetUnderline())
	}
}

func TestConnectionRowReservesScreenSpace(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			m := sendRune(cronFixture(t), '1')
			m.width, m.height = 200, 40
			if split {
				m.layout = splitlayout.Split
			}
			iss := testIssue("abc1")
			m.detail.issue = &iss
			m.list.issues = []Issue{iss}
			m.sseStatus = sseConnected
			full := m.listRenderedDataRows()
			m.sseStatus = sseReconnecting
			require.Equal(t, full-1, m.listRenderedDataRows())
			require.Len(t, strings.Split(m.viewContent(), "\n"), m.height)
			m = sendRune(m, '2')
			require.Len(t, strings.Split(m.viewContent(), "\n"), m.height)
		})
	}
}

func TestSharedTabHeaderShortAndNarrow(t *testing.T) {
	for _, size := range [][2]int{{100, 1}, {100, 4}, {60, 24}, {200, 40}} {
		m := sendRune(cronFixture(t), '1')
		m.width, m.height = size[0], size[1]
		m.sseStatus = sseReconnecting
		body := stripANSI(m.viewContent())
		first, _, _ := strings.Cut(body, "\n")
		require.Contains(t, first, "[kata]")
		require.LessOrEqual(t, len(strings.Split(body, "\n")), m.height)
	}
}

func TestSharedTabHeaderKeepsShortDetailContent(t *testing.T) {
	m := sendRune(cronFixture(t), '1')
	m.view, m.height = viewDetail, 4
	iss := testIssue("abc1")
	iss.Title = "Keep this issue visible"
	m.detail.issue = &iss
	body := stripANSI(m.viewContent())
	require.Contains(t, body, "[kata]")
	require.Contains(t, body, iss.Title)
	require.LessOrEqual(t, len(strings.Split(body, "\n")), m.height)
}

func TestConnectionRowMovesMouseTargets(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			m := resizeModel(newTestModel(), 160, 40)
			m.opts.Mouse = true
			m.list.issues = makeTestIssues(5)
			if split {
				m.layout = splitlayout.Split
			}
			healthy, _ := updateModel(m, mouseLeftClick(4, 6))
			m.sseStatus = sseReconnecting
			unhealthy, _ := updateModel(m, mouseLeftClick(4, 7))
			require.Equal(t, healthy.list.selectedUID, unhealthy.list.selectedUID)
			require.NotEmpty(t, unhealthy.list.selectedUID)
		})
	}
}

func TestConnectionRowRefreshesViewportCaches(t *testing.T) {
	m := resizeModel(newTestModel(), 120, 30)
	m.list.issues = makeTestIssues(40)
	iss := testIssue("abc1")
	m.detail.issue = &iss
	rows := m.list.pageRows
	m, _ = updateModel(m, sseStatusMsg{gen: m.connGen, state: sseReconnecting})
	require.Equal(t, rows-1, m.list.pageRows)
	require.Equal(t, 29, m.detail.lastTermHeight)
	m, _ = updateModel(m, sseStatusMsg{gen: m.connGen, state: sseConnected})
	require.Equal(t, rows, m.list.pageRows)
	require.Equal(t, 30, m.detail.lastTermHeight)
}

func TestExpiredConnectionRowRefreshesViewportCaches(t *testing.T) {
	m := resizeModel(newTestModel(), 120, 30)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m.toastNow = func() time.Time { return now }
	expiry := now.Add(time.Minute)
	m, _ = updateModel(m, authCapabilitiesMsg{connGen: m.connGen, auth: AuthInfo{Kind: "token", Actor: "member", ExpiresAt: &expiry}})
	require.Equal(t, 30, m.detail.lastTermHeight)
	now = expiry
	m, _ = updateModel(m, activeCredentialExpiredMsg{connGen: m.connGen, expiresAt: expiry})
	require.Equal(t, 29, m.detail.lastTermHeight)
	require.Contains(t, connectionAttentionRow(t, m), "Credential expired")
}

func TestConnectionRowKeepsScrolledFederationMouseWindow(t *testing.T) {
	m := resizeModel(newTestModel(), 120, 20)
	m.view, m.opts.Mouse = viewFederation, true
	for i := range 30 {
		m.federation.statuses = append(m.federation.statuses, federationStatusFixture(fmt.Sprintf("project-%02d", i), "spoke"))
	}
	m.federation.cursor = 20
	m.sseStatus = sseReconnecting
	// The alert leaves eight rows, with one third reserved above the cursor.
	// A cursor at 20 shows 18..25, so the first row must select index 18.
	m, _ = updateModel(m, mouseLeftClick(4, federationFirstRowY+1))
	require.Equal(t, 18, m.federation.cursor)
}
