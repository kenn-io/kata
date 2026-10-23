package tui

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kit/tui/splitlayout"
)

type attentionTestAPI struct {
	KataAPI
	rows                           []attentionRequest
	skipped, listCalls, clearCalls int
	lastProjectID                  *int64
	recipient                      string
	listErr, clearErr              error
	clearProjectID                 int64
	clearRef, clearKey, clearActor string
	clearIfValue                   jsontext.Value
}

func (a *attentionTestAPI) ListAttentionRequests(_ context.Context, pid *int64, recipient string) ([]attentionRequest, int, error) {
	a.listCalls++
	a.lastProjectID = pid
	a.recipient = recipient
	return a.rows, a.skipped, a.listErr
}
func (a *attentionTestAPI) ClearAttentionRequest(_ context.Context, pid int64, ref, key string, raw jsontext.Value, actor string) error {
	a.clearCalls++
	a.clearProjectID = pid
	a.clearRef = ref
	a.clearKey = key
	a.clearIfValue = raw
	a.clearActor = actor
	return a.clearErr
}

type attentionCapabilitiesTestAPI struct {
	*attentionTestAPI
	tokenListCalls int
}

func (a *attentionCapabilitiesTestAPI) ListTokens(context.Context) ([]TokenInfo, time.Time, error) {
	a.tokenListCalls++
	return []TokenInfo{{ID: 42, Actor: "member"}}, time.Time{}, nil
}

type attentionProjectsTestAPI struct {
	*attentionTestAPI
	projectListCalls int
}

func (a *attentionProjectsTestAPI) ListProjectsWithStats(context.Context) ([]ProjectSummaryWithStats, error) {
	a.projectListCalls++
	return []ProjectSummaryWithStats{{
		ID:    7,
		Name:  "example-project",
		Stats: &ProjectStatsSummary{Open: 3, Closed: 1},
	}}, nil
}

type attentionBrowseTestAPI struct {
	*attentionTestAPI
	allIssues []Issue
	allCalls  int
}

func (a *attentionBrowseTestAPI) ListAllIssues(context.Context, ListFilter) ([]Issue, error) {
	a.allCalls++
	return a.allIssues, nil
}

func attentionFixture(t *testing.T) (Model, *attentionTestAPI) {
	t.Helper()
	t.Setenv("KATA_INBOX_USER", "")
	m := newTestModel()
	m.width = 120
	m.height = 40
	m.list.actor = "alex"
	m.scope.projectName = "example-project"
	a := &attentionTestAPI{rows: []attentionRequest{{issue: testIssue("abc4"), from: "sam", message: "Please review", key: notification.MetadataKey("alex"), raw: jsontext.Value(`{"from":"sam","message":"Please review"}`)}, {issue: testIssue("def5", inProject(9)), from: "system", message: "Deadline reached", key: notification.MetadataKey("alex"), raw: jsontext.Value(`{"from":"system","message":"Deadline reached"}`)}}}
	m.api = a
	return m, a
}
func loadAttention(m Model, cmd tea.Cmd) Model {
	if cmd != nil {
		m, _ = updateModel(m, cmd())
	}
	return m
}

func TestAttentionViewToggle(t *testing.T) {
	for _, origin := range []viewID{viewList, viewDetail, viewProjects, viewDaemons, viewFederation, viewCredentials} {
		t.Run(fmt.Sprint(origin), func(t *testing.T) {
			m, a := attentionFixture(t)
			m.view = origin
			original := m.scope
			filter := m.list.filter
			m, cmd := updateModel(m, keyRune('T'))
			require.Equal(t, viewAttention, m.view)
			m = loadAttention(m, cmd)
			require.Equal(t, 1, a.listCalls)
			require.Equal(t, original, m.scope)
			m = sendRune(m, 'T')
			require.Equal(t, origin, m.view)
			require.Equal(t, filter, m.list.filter)
			m, _ = updateModel(m, keyRune('T'))
			m = sendKey(m, tea.KeyEscape)
			require.Equal(t, origin, m.view)
		})
	}
	m, _ := attentionFixture(t)
	m.modal = modalQuitConfirm
	m = sendRune(m, 'T')
	require.NotEqual(t, viewAttention, m.view)
	m, _ = attentionFixture(t)
	m.input.kind = inputSearchBar
	m = sendRune(m, 'T')
	require.NotEqual(t, viewAttention, m.view)
}

func TestAttentionRecipient(t *testing.T) {
	for _, tc := range []struct {
		env, auth, want string
		bad             bool
	}{{"alex/reviewer", "other", "alex/reviewer", false}, {"  ", "member", "member", false}, {"", "", "alex", false}, {"bad\x01", "member", "", true}} {
		t.Run(tc.env+tc.auth, func(t *testing.T) {
			m, a := attentionFixture(t)
			t.Setenv("KATA_INBOX_USER", tc.env)
			m.activeAuth.Actor = tc.auth
			m.authCapabilitiesReady = true
			m, cmd := m.transitionToAttention()
			m = loadAttention(m, cmd)
			if tc.bad {
				require.Error(t, m.attention.recipientErr)
				require.Zero(t, a.listCalls)
			} else {
				require.Equal(t, tc.want, a.recipient)
			}
		})
	}
	m, a := attentionFixture(t)
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = false
	m, cmd := m.transitionToAttention()
	require.True(t, m.attention.capabilityPending)
	require.Zero(t, a.listCalls)
	require.NotNil(t, cmd)
	m, cmd = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
	m = loadAttention(m, cmd)
	require.Equal(t, 1, a.listCalls)
	require.Equal(t, "member", a.recipient)
}

func TestAttentionScope(t *testing.T) {
	for _, sc := range []scope{{projectID: 7}, {allProjects: true}, {}, {projectID: 9, inbox: true}} {
		m, a := attentionFixture(t)
		m.scope = sc
		m, cmd := m.transitionToAttention()
		_ = loadAttention(m, cmd)
		if sc.projectID == 0 || sc.allProjects {
			require.Nil(t, a.lastProjectID)
		} else {
			require.Equal(t, sc.projectID, *a.lastProjectID)
		}
	}
}
func TestAttentionLoaded(t *testing.T) {
	m, a := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = sendRune(m, 'j')
	require.Equal(t, a.rows[1].issue.UID, m.attention.selectedUID)
	a.rows = []attentionRequest{a.rows[1], a.rows[0]}
	m, cmd = updateModel(m, keyRune('r'))
	m = loadAttention(m, cmd)
	require.Zero(t, m.attention.cursor)
	a.listErr = errors.New("boom")
	m, cmd = updateModel(m, keyRune('r'))
	m = loadAttention(m, cmd)
	require.Error(t, m.attention.err)
	require.Len(t, m.attention.rows, 2)
	a.listErr = nil
	a.rows = a.rows[1:]
	m, cmd = updateModel(m, keyRune('r'))
	m = loadAttention(m, cmd)
	require.Zero(t, m.attention.cursor)
	require.Equal(t, a.rows[0].issue.UID, m.attention.selectedUID)
	a.rows = nil
	a.skipped = 2
	m, cmd = updateModel(m, keyRune('r'))
	m = loadAttention(m, cmd)
	require.Empty(t, m.attention.rows)
	require.Equal(t, 2, m.attention.skipped)
}
func TestAttentionStaleReply(t *testing.T) {
	m, _ := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m.connGen = 3
	for _, msg := range []attentionLoadedMsg{{connGen: 2, gen: m.attention.gen}, {connGen: 3, gen: m.attention.gen - 1}} {
		next, _ := m.handleAttentionLoaded(msg)
		require.Equal(t, m.attention.rows, next.attention.rows)
	}
	oldGen := m.attention.gen
	m = sendKey(m, tea.KeyEscape)
	next, _ := m.handleAttentionLoaded(attentionLoadedMsg{connGen: 3, gen: oldGen})
	require.Equal(t, m.attention.rows, next.attention.rows)
}
func TestAttentionGlobalExit(t *testing.T) {
	for _, r := range []rune{'P', 'I', 'D', 'F', 'C'} {
		m, _ := attentionFixture(t)
		m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}
		m, _ = m.transitionToAttention()
		m = sendRune(m, r)
		require.NotEqual(t, viewAttention, m.view)
	}
	m, _ := attentionFixture(t)
	m, _ = m.transitionToAttention()
	gen := m.attention.gen
	m = sendRune(m, '?')
	require.Equal(t, viewHelp, m.view)
	m = sendRune(m, '?')
	require.Equal(t, viewAttention, m.view)
	require.Equal(t, gen, m.attention.gen)
}
func TestAttentionDaemonSwitch(t *testing.T) {
	m, _ := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	gen := m.attention.gen
	m, _ = m.installDaemonConnection(daemonConnection{api: &Client{}})
	require.Empty(t, m.attention.rows)
	require.Greater(t, m.attention.gen, gen)
}

func TestAttentionDetailReturn(t *testing.T) {
	for _, width := range []int{80, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m, a := attentionFixture(t)
			m = resizeModel(m, width, 40)
			m.scope.allProjects = true
			m.view = viewProjects
			m, cmd := m.transitionToAttention()
			m = loadAttention(m, cmd)
			m = sendRune(m, 'j')
			selected := a.rows[1]
			originalReturn := m.attention.returnView
			m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Equal(t, viewDetail, m.view)
			require.NotNil(t, cmd)
			require.Equal(t, selected.issue.UID, m.detail.issue.UID)
			require.Equal(t, selected.issue.ProjectID, m.detail.scopePID)
			// A nested reference uses the ordinary detail navigation stack.
			next, _ := m.handleJumpDetail(jumpDetailMsg{ref: "nested", projectID: 7})
			m = next.(Model)
			require.Len(t, m.detail.navStack, 1)
			m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			require.Empty(t, m.detail.navStack)
			require.Equal(t, selected.issue.UID, m.detail.issue.UID)
			m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
			if m.view == viewDetail {
				require.NotNil(t, cmd)
				m, cmd = updateModel(m, cmd())
			}
			require.Equal(t, viewAttention, m.view)
			m = loadAttention(m, cmd)
			require.Equal(t, selected.issue.UID, m.attention.selectedUID)
			require.Equal(t, originalReturn, m.attention.returnView)
			require.Equal(t, 2, a.listCalls)
		})
	}
}

func TestAttentionRestoresDetailNavigationAndFocusAfterResize(t *testing.T) {
	m, a := attentionFixture(t)
	origin := testIssue("origin")
	prior := testIssue("prior")
	m.view, m.prevView = viewDetail, viewList
	m.detail = detailModel{
		issue:     &origin,
		scopePID:  7,
		activeTab: tabEvents,
		navStack:  []detailModel{{issue: &prior, scopePID: 7}},
	}
	m.focus = focusList // stacked layouts do not require a detail-focused pane.

	m, cmd := updateModel(m, keyRune('T'))
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, a.rows[0].issue.UID, m.detail.issue.UID)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m = loadAttention(m, cmd)
	require.Equal(t, viewAttention, m.view)

	// Attention stays in front while the terminal changes from stacked to split.
	m = resizeModel(m, 160, 40)
	require.Equal(t, viewAttention, m.view)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewDetail, m.view)
	require.Equal(t, viewList, m.prevView)
	require.Equal(t, focusDetail, m.focus,
		"the restored detail view must own key dispatch in split layout")
	require.Equal(t, splitlayout.Split, m.layout)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, origin.UID, m.detail.issue.UID)
	require.Equal(t, tabEvents, m.detail.activeTab)
	require.Len(t, m.detail.navStack, 1)
	require.Equal(t, prior.UID, m.detail.navStack[0].issue.UID)
	require.NotNil(t, cmd, "the retained detail must be refreshed after Attention")
}

func TestAttentionKeepsFocusedDetailVisibleWhenResizedToStacked(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	require.Equal(t, splitlayout.Split, m.layout)
	origin := testIssue("origin")
	prior := testIssue("prior")
	m.view, m.prevView, m.focus = viewList, viewList, focusDetail
	m.detail = detailModel{
		issue:    &origin,
		scopePID: origin.ProjectID,
		navStack: []detailModel{{issue: &prior, scopePID: prior.ProjectID}},
	}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = resizeModel(m, 80, 24)
	require.Equal(t, splitlayout.Stacked, m.layout)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewDetail, m.view,
		"the pane that had detail focus must remain visible in stacked layout")
	require.Equal(t, focusDetail, m.focus)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, origin.UID, m.detail.issue.UID)
	require.Len(t, m.detail.navStack, 1)
	require.Equal(t, prior.UID, m.detail.navStack[0].issue.UID)
}

func TestAttentionPreservesAuxiliaryViewWithDetailFocusAfterResize(t *testing.T) {
	for _, tc := range []struct {
		name string
		view viewID
	}{
		{"projects", viewProjects},
		{"daemons", viewDaemons},
		{"federation", viewFederation},
		{"credentials", viewCredentials},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := attentionFixture(t)
			m = resizeModel(m, 160, 40)
			origin := testIssue("origin")
			m.view, m.prevView, m.focus = tc.view, viewList, focusDetail
			m.detail = detailModel{issue: &origin, scopePID: origin.ProjectID}

			m, cmd := m.transitionToAttention()
			m = loadAttention(m, cmd)
			m = resizeModel(m, 80, 24)
			m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

			require.Equal(t, tc.view, m.view,
				"Attention should restore the auxiliary foreground that it covered")
			require.Equal(t, origin.UID, m.detail.issue.UID)
		})
	}
}

func TestAttentionRestoresDetailViewportAfterResize(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	origin := testIssue("origin")
	origin.Body = strings.Repeat("Long detail text for viewport wrapping. ", 400)
	m.view, m.prevView, m.focus = viewDetail, viewList, focusDetail
	m.detail = detailModel{issue: &origin, scopePID: origin.ProjectID}
	m.detail = m.applyDetailViewportCache(m.detail)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = resizeModel(m, 200, 50)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	expected := m.applyDetailViewportCache(m.detail)
	require.Equal(t, expected.lastTermWidth, m.detail.lastTermWidth)
	require.Equal(t, expected.lastTermHeight, m.detail.lastTermHeight)
	require.Equal(t, expected.lastDetailWidth, m.detail.lastDetailWidth)
	require.Equal(t, expected.lastDetailHeight, m.detail.lastDetailHeight)
	width, visible, ok := expected.viewportDims()
	require.True(t, ok)
	docLines, _ := expected.detailDocumentLines(width, expected.scrollChrome())
	require.Equal(t, viewportMaxStart(len(docLines), visible), m.detail.scrollViewportToEnd().scroll,
		"scroll-to-end must use the resized detail viewport")
}

func TestAttentionReconcilesListFocusedSplitDetailOnReturn(t *testing.T) {
	t.Run("changed highlight", func(t *testing.T) {
		m, _ := attentionFixture(t)
		m = resizeModel(m, 160, 40)
		original := testIssue("original")
		current := testIssue("current")
		m.view, m.focus = viewList, focusList
		m.list.issues = []Issue{original}
		m.detail = detailModel{issue: &original, scopePID: original.ProjectID}
		m.detail = m.applyDetailViewportCache(m.detail)
		m, cmd := m.transitionToAttention()
		m = loadAttention(m, cmd)
		m.list.issues = []Issue{current}
		m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

		require.Equal(t, viewList, m.view)
		require.NotNil(t, m.detail.issue)
		require.Equal(t, current.UID, m.detail.issue.UID,
			"list-focused browsing must follow the current highlight after Attention")
		require.NotNil(t, cmd)
	})

	t.Run("widened while Attention is open", func(t *testing.T) {
		m, _ := attentionFixture(t)
		current := testIssue("current")
		m.view, m.focus = viewList, focusList
		m.list.issues = []Issue{current}
		m.detail = newDetailModel()
		m, cmd := m.transitionToAttention()
		m = loadAttention(m, cmd)
		m = resizeModel(m, 160, 40)
		require.Equal(t, splitlayout.Split, m.layout)
		m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

		require.Equal(t, viewList, m.view)
		require.NotNil(t, m.detail.issue,
			"widening while Attention is open must populate the split detail pane")
		require.Equal(t, current.UID, m.detail.issue.UID)
		require.NotNil(t, cmd)
	})
}

func TestAttentionRefreshesProjectsWhenReturning(t *testing.T) {
	m, a := attentionFixture(t)
	projectsAPI := &attentionProjectsTestAPI{attentionTestAPI: a}
	m.api = projectsAPI
	m.view = viewProjects
	m.projectStats = map[int64]ProjectStatsSummary{7: {Open: 1}}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewProjects, m.view)
	require.NotNil(t, cmd, "returning to Projects must fetch current project statistics")
	msg, ok := cmd().(projectsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	require.NotNil(t, msg.stats)
	require.Equal(t, 1, projectsAPI.projectListCalls)
}

func TestAttentionProjectsOriginInitializesListBeforePaneNavigation(t *testing.T) {
	for _, tc := range []struct {
		name string
		view viewID
		act  func(Model) (Model, tea.Cmd)
	}{
		{
			name: "tab to list",
			view: viewList,
			act: func(m Model) (Model, tea.Cmd) {
				return updateModel(m, tea.KeyPressMsg{Code: tea.KeyTab})
			},
		},
		{
			name: "mouse click list pane",
			view: viewDetail,
			act: func(m Model) (Model, tea.Cmd) {
				m.opts.Mouse = true
				return updateModel(m, mouseLeftClick(4, 4))
			},
		},
		{
			name: "mouse wheel list pane",
			view: viewDetail,
			act: func(m Model) (Model, tea.Cmd) {
				m.opts.Mouse = true
				return updateModel(m, mouseWheelDownAt(4))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, attentionAPI := attentionFixture(t)
			browseAPI := &attentionBrowseTestAPI{
				attentionTestAPI: attentionAPI,
				allIssues:        []Issue{attentionAPI.rows[0].issue},
			}
			m.api = browseAPI
			m.scope = scope{}
			m.view = viewProjects
			m = resizeModel(m, 160, 40)

			m, cmd := updateModel(m, keyRune('T'))
			m = loadAttention(m, cmd)
			m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Equal(t, viewDetail, m.view)
			require.True(t, m.attention.returnToAttention)

			m, cmd = tc.act(m)
			require.Equal(t, tc.view, m.view)
			require.Equal(t, focusList, m.focus)
			require.True(t, m.scope.allProjects,
				"leaving an unscoped Projects-origin detail must establish a usable all-projects list")
			require.False(t, m.attention.returnToAttention)
			require.Nil(t, m.attention.returnContext)
			require.True(t, m.list.loading)
			require.NotNil(t, cmd, "pane navigation must dispatch the list fetch")

			loaded, ok := cmd().(initialFetchMsg)
			require.True(t, ok, "pane navigation should fetch issues for the new scope")
			require.True(t, loaded.dispatchKey.allProjects)
			require.Equal(t, 1, browseAPI.allCalls)
			m, _ = updateModel(m, loaded)
			require.False(t, m.list.loading)
			require.Equal(t, browseAPI.allIssues, m.list.issues)
		})
	}
}

func TestAttentionRestoresStackedDetailPaneFocusAfterAuxResize(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 100, 40)
	origin := testIssue("origin")
	m.list.issues = []Issue{testIssue("first"), testIssue("second")}
	m.list.cursor = 0
	m.list.selectedUID = m.list.issues[0].UID
	m.view, m.prevView, m.focus = viewDetail, viewList, focusList
	m.detail = detailModel{issue: &origin, scopePID: origin.ProjectID}
	m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}

	m = sendRune(m, 'D')
	require.Equal(t, viewDaemons, m.view)
	require.Equal(t, viewDetail, m.prevView)
	m, cmd := updateModel(m, keyRune('T'))
	m = loadAttention(m, cmd)
	m = resizeModel(m, 160, 40)
	require.Equal(t, splitlayout.Split, m.layout)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewDaemons, m.view)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewDetail, m.view)
	require.Equal(t, focusDetail, m.focus,
		"restoring the auxiliary view's saved detail target into split layout must focus detail")
	m = sendRune(m, 'j')
	require.Zero(t, m.list.cursor,
		"navigation after restoring stacked detail must not move the hidden issue list")
	require.Equal(t, origin.UID, m.detail.issue.UID)
}

func TestAttentionCompletesPendingCredentialsCapabilities(t *testing.T) {
	m, a := attentionFixture(t)
	credentialsAPI := &attentionCapabilitiesTestAPI{attentionTestAPI: a}
	m.api = credentialsAPI
	m.view, m.prevView = viewCredentials, viewDaemons
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = false
	m.credentials = credentialAuditState{capabilityPending: true, loading: true, gen: 7}
	m, cmd := m.transitionToAttention()
	require.True(t, m.attention.capabilityPending)
	require.NotNil(t, cmd)

	m, cmd = m.handleAuthCapabilities(authCapabilitiesMsg{
		connGen: m.connGen,
		auth:    AuthInfo{Actor: "member", TokenAuditRead: true},
	})
	require.False(t, m.credentials.capabilityPending)
	require.True(t, m.credentials.available)
	require.True(t, m.credentials.loading)
	require.NotNil(t, cmd)

	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok, "the capability response must continue both hidden view requests")
	var gotCredentialResponse, gotAttentionResponse bool
	for _, child := range batch {
		switch child().(type) {
		case credentialsLoadedMsg:
			gotCredentialResponse = true
		case attentionLoadedMsg:
			gotAttentionResponse = true
		}
	}
	require.True(t, gotCredentialResponse)
	require.True(t, gotAttentionResponse)
	require.Equal(t, 1, credentialsAPI.tokenListCalls)
	require.Equal(t, 1, a.listCalls)
}

func TestAttentionHelpDoesNotReplaceDaemonsBackTarget(t *testing.T) {
	m, _ := attentionFixture(t)
	m.view, m.prevView = viewDaemons, viewList
	m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}
	m, cmd := updateModel(m, keyRune('T'))
	m = loadAttention(m, cmd)
	m = sendRune(m, '?')
	m = sendRune(m, '?')
	m = sendKey(m, tea.KeyEscape)
	require.Equal(t, viewDaemons, m.view)
	require.Equal(t, viewList, m.prevView)
	m = sendKey(m, tea.KeyEscape)
	require.Equal(t, viewList, m.view,
		"Esc from Daemons must return to its original view, not reopen Attention")
}

func TestAttentionRestartsCanceledSplitDetailFollow(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	issue := testIssue("follow")
	m.list.issues = []Issue{issue}
	m, pending := m.scheduleDetailFollow()
	require.NotNil(t, pending)
	pendingGen := m.nextDetailFollowGen

	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	_, stale := m.handleDetailFollowTick(detailFollowTickMsg{gen: pendingGen})
	require.Nil(t, stale, "entering Attention must cancel the previous debounce tick")
	m, resumed := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewList, m.view)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, issue.UID, m.detail.issue.UID)
	require.True(t, m.detail.commentsLoading)
	require.True(t, m.detail.eventsLoading)
	require.True(t, m.detail.linksLoading)
	require.NotNil(t, resumed,
		"returning must dispatch replacement work for the retained split detail")
}

func TestAttentionInspectDoesNotRetargetDetailOnListRefresh(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	ordinary := testIssue("ordinary")
	inspected := testIssue("inspected")
	m.list.issues = []Issue{ordinary}
	m.detail = detailModel{issue: &inspected, scopePID: inspected.ProjectID}
	m.view = viewDetail
	m.focus = focusDetail
	m.attention.returnToAttention = true

	// A list refresh removes the ordinary selection and highlights another row
	// while the detail pane is inspecting a request opened from Attention.
	replacement := testIssue("replacement")
	m.list.issues = []Issue{replacement}
	m, cmd := m.reconcileSplitDetailAfterRefetch(ordinary.ProjectID, ordinary.UID, true)
	require.Nil(t, cmd)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, inspected.UID, m.detail.issue.UID)

	m.list.issues = []Issue{replacement}
	m, cmd = m.scheduleDetailFollow()
	require.Nil(t, cmd, "deferred detail-follow work must not retarget the inspected request")
	require.Equal(t, inspected.UID, m.detail.issue.UID)
}

func TestAttentionCredentialsRefreshContinuesWhileHidden(t *testing.T) {
	m, _ := attentionFixture(t)
	m.view, m.prevView = viewCredentials, viewDaemons
	m.credentials = credentialAuditState{available: true, loading: true, gen: 7}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)

	loaded, tick := m.handleCredentialsLoaded(credentialsLoadedMsg{
		connGen: m.connGen, gen: 7, tokens: []TokenInfo{{ID: 4, Actor: "example-agent"}},
	})
	require.False(t, loaded.credentials.loading)
	require.True(t, loaded.credentials.refreshScheduled)
	require.NotNil(t, tick, "a load that finishes under Attention must keep its timer")

	refreshing, fetch := loaded.handleCredentialsRefreshTick(credentialsRefreshTickMsg{
		connGen: m.connGen, gen: 7,
	})
	require.True(t, refreshing.credentials.loading)
	require.False(t, refreshing.credentials.refreshScheduled)
	require.NotNil(t, fetch, "a refresh tick must run while Credentials is hidden")

	refreshed, nextTick := refreshing.handleCredentialsLoaded(credentialsLoadedMsg{
		connGen: m.connGen, gen: 7, tokens: []TokenInfo{{ID: 5, Actor: "example-agent"}},
	})
	require.False(t, refreshed.credentials.loading)
	require.True(t, refreshed.credentials.refreshScheduled)
	require.NotNil(t, nextTick)

	back, _ := updateModel(refreshed, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewCredentials, back.view)
	require.Equal(t, viewDaemons, back.prevView,
		"Attention must preserve Credentials' original back target")
	require.True(t, back.credentials.refreshScheduled)
}
func TestAttentionReturnFlagClears(t *testing.T) {
	for _, r := range []rune{'P', 'I', 'D', 'F', 'C'} {
		m, _ := attentionFixture(t)
		m.daemonTargets = []daemonTarget{{Local: true}}
		m, cmd := m.transitionToAttention()
		m = loadAttention(m, cmd)
		m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.True(t, m.attention.returnToAttention)
		m = sendRune(m, r)
		require.False(t, m.attention.returnToAttention)
	}
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = sendRune(m, '?')
	m = sendRune(m, '?')
	require.True(t, m.attention.returnToAttention)
	m = sendKey(m, tea.KeyTab)
	require.Equal(t, focusList, m.focus)
	require.Equal(t, viewList, m.view)
	require.False(t, m.attention.returnToAttention)
}
func TestAttentionExitRestoresDetailAndClearsReturnContext(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	issue := testIssue("detail")
	m.view, m.prevView, m.focus = viewDetail, viewList, focusDetail
	m.detail = detailModel{issue: &issue, scopePID: issue.ProjectID}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)

	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewDetail, m.view)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, issue.UID, m.detail.issue.UID)
	require.Nil(t, m.attention.returnContext,
		"leaving Attention must clear its saved return context")
}

func TestAttentionRestoresAuxiliaryListDetailAfterWiden(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 100, 40)
	issue := testIssue("ordinary")
	m.list.issues = []Issue{issue}
	m.list.selectedUID = issue.UID
	m.detail = newDetailModel()
	m.daemonTargets = []daemonTarget{{Local: true}}
	m = sendRune(m, 'D')
	require.Equal(t, viewDaemons, m.view)
	require.Equal(t, viewList, m.prevView)

	m, cmd := updateModel(m, keyRune('T'))
	m = loadAttention(m, cmd)
	m = resizeModel(m, 160, 40)
	// The resize happens while Attention covers Daemons, so the list's
	// split detail still needs to be reconciled when that saved context returns.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewDaemons, m.view)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewList, m.view)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, issue.UID, m.detail.issue.UID)
}

func TestAttentionSyncsInFlightDetailIntoReturnContext(t *testing.T) {
	m, _ := attentionFixture(t)
	issue := testIssue("loading")
	m.view, m.prevView, m.focus = viewDetail, viewList, focusDetail
	m.detail = detailModel{gen: 42, loading: true, commentsLoading: true}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)

	m, _ = updateModel(m, detailFetchedMsg{gen: 42, issue: &issue})
	require.NotNil(t, m.detail.issue, "the live detail should accept its matching response")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewDetail, m.view)
	require.NotNil(t, m.detail.issue)
	require.Equal(t, issue.UID, m.detail.issue.UID)
	require.False(t, m.detail.loading,
		"a matching response received under Attention must not restore a stuck loading snapshot")
}

func TestAttentionRestoresListFocusFromSplitDetailView(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	issue := testIssue("focused-list")
	m.view, m.focus = viewDetail, focusList
	m.detail = detailModel{issue: &issue, scopePID: issue.ProjectID}

	m, cmd := m.transitionToAttention()
	require.Equal(t, viewList, m.attention.returnContext.foreground)
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, focusList, m.focus,
		"Attention must restore list focus when the detail view was not focused")
}

func TestAttentionRestoresAuxiliaryFocusedDetailWhenResizedToStacked(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	origin := testIssue("origin")
	prior := testIssue("prior")
	m.view, m.prevView, m.focus = viewDaemons, viewList, focusDetail
	m.detail = detailModel{
		issue:    &origin,
		scopePID: origin.ProjectID,
		navStack: []detailModel{{issue: &prior, scopePID: prior.ProjectID}},
	}
	m.daemonTargets = []daemonTarget{{Local: true}}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = resizeModel(m, 80, 24)

	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewDaemons, m.view)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})

	require.Equal(t, viewDetail, m.view,
		"leaving Daemons after Attention must return to the previously focused detail")
	require.NotNil(t, m.detail.issue)
	require.Equal(t, origin.UID, m.detail.issue.UID)
	require.Len(t, m.detail.navStack, 1)
	require.Equal(t, prior.UID, m.detail.navStack[0].issue.UID)
}

func TestAttentionClear(t *testing.T) {
	m, a := attentionFixture(t)
	m.scope.allProjects = true
	t.Setenv("KATA_INBOX_USER", "alex/reviewer")
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = sendRune(m, 'j')
	selected := a.rows[1]
	m, cmd = updateModel(m, keyRune('c'))
	require.True(t, m.attention.clearing)
	require.Len(t, m.attention.rows, 2)
	_, again := updateModel(m, keyRune('c'))
	require.Nil(t, again)
	_, refresh := updateModel(m, keyRune('r'))
	require.Nil(t, refresh)
	msg := cmd()
	require.Equal(t, 1, a.clearCalls)
	require.Equal(t, int64(9), a.clearProjectID)
	require.Equal(t, selected.issue.UID, a.clearRef)
	require.Equal(t, selected.key, a.clearKey)
	require.Equal(t, string(selected.raw), string(a.clearIfValue))
	require.Equal(t, m.list.actor, a.clearActor)
	a.rows = a.rows[:1]
	m, cmd = updateModel(m, msg)
	m = loadAttention(m, cmd)
	require.False(t, m.attention.clearing)
	require.Len(t, m.attention.rows, 1)
	require.Zero(t, m.attention.cursor)
	require.Equal(t, 2, a.listCalls)
}
func TestAttentionClearConflict(t *testing.T) {
	m, a := attentionFixture(t)
	a.clearErr = &APIError{Status: 409, Code: "metadata_guard_failed"}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, cmd = updateModel(m, keyRune('c'))
	m, cmd = updateModel(m, cmd())
	require.Equal(t, "Request changed; review it before clearing", m.toast.text)
	require.False(t, m.attention.clearing)
	batch := cmd().(tea.BatchMsg)
	m = loadAttention(m, batch[0])
	require.Equal(t, 2, a.listCalls)
	require.Equal(t, 1, a.clearCalls)
	require.Len(t, m.attention.rows, 2)
}
func TestAttentionClearFailure(t *testing.T) {
	for _, err := range []error{&mutationPreflightError{errors.New("requires 0.13.0")}, &APIError{Status: 403, Code: "forbidden"}, errors.New("connection lost")} {
		t.Run(err.Error(), func(t *testing.T) {
			m, a := attentionFixture(t)
			a.clearErr = err
			m, cmd := m.transitionToAttention()
			m = loadAttention(m, cmd)
			m, cmd = updateModel(m, keyRune('c'))
			m, cmd = updateModel(m, cmd())
			require.NotNil(t, m.toast)
			require.Len(t, m.attention.rows, 2)
			require.False(t, m.attention.clearing)
			if err.Error() == "connection lost" {
				batch := cmd().(tea.BatchMsg)
				m = loadAttention(m, batch[0])
				require.Equal(t, 2, a.listCalls)
			} else {
				require.Equal(t, 1, a.listCalls)
			}
		})
	}
}
func TestAttentionClearGate(t *testing.T) {
	m, a := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = false
	m, cmd = updateModel(m, keyRune('c'))
	require.NotNil(t, cmd)
	require.Contains(t, m.toast.text, "Checking daemon permissions")
	require.Zero(t, a.clearCalls)
	m.authCapabilitiesReady = true
	m.issueScoped = true
	m.scopedWritable = false
	_, cmd = updateModel(m, keyRune('c'))
	require.Nil(t, cmd)
	m.issueScoped = false
	m.attention.err = errors.New("refresh failed")
	m, cmd = updateModel(m, keyRune('c'))
	require.NotNil(t, cmd)
	_ = cmd()
	require.Equal(t, 1, a.clearCalls)
	m.attention.clearing = false
	m.attention.rows = nil
	_, cmd = updateModel(m, keyRune('c'))
	require.Nil(t, cmd)
}
func TestAttentionClearRejectsPriorConnectionCompletion(t *testing.T) {
	m, oldAPI := attentionFixture(t)
	m.connGen = 1
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, oldClearCmd := updateModel(m, keyRune('c'))
	require.True(t, m.attention.clearing)
	oldCompletion := oldClearCmd()
	require.Equal(t, 1, oldAPI.clearCalls)

	newModel, newAPI := attentionFixture(t)
	previousConnGen := m.connGen
	m, _ = m.installDaemonConnection(daemonConnection{
		api:  &Client{},
		init: bootInit{view: viewList, scope: newModel.scope},
	})
	require.Equal(t, previousConnGen+1, m.connGen)
	m.api = newAPI
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{
		connGen: m.connGen,
		auth:    AuthInfo{Actor: "alex"},
	})
	m, cmd = m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, newClearCmd := updateModel(m, keyRune('c'))
	require.True(t, m.attention.clearing)
	require.NotNil(t, newClearCmd)
	newClearGen := m.attention.clearGen
	newRows := m.attention.rows
	_ = newClearCmd()
	require.Equal(t, 1, newAPI.clearCalls)

	m, cmd = updateModel(m, oldCompletion)
	require.Nil(t, cmd)
	require.True(t, m.attention.clearing,
		"a completion from the previous daemon must not clear the current pending operation")
	require.Equal(t, newClearGen, m.attention.clearGen)
	require.Equal(t, newRows, m.attention.rows)
}
func TestAttentionClearFencesEarlierRead(t *testing.T) {
	m, a := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, cmd = updateModel(m, keyRune('r'))
	oldRead := cmd()
	m, cmd = updateModel(m, keyRune('c'))
	cleared := cmd()
	a.rows = a.rows[1:]
	m, cmd = updateModel(m, cleared)
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, oldRead)
	require.Len(t, m.attention.rows, 1)
	require.Equal(t, a.rows[0].issue.UID, m.attention.rows[0].issue.UID)
	require.Equal(t, 3, a.listCalls)
}

func TestAttentionClearDetailReturn(t *testing.T) {
	m, _ := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, clearCmd := updateModel(m, keyRune('c'))
	require.True(t, m.attention.clearing)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, _ = updateModel(m, clearCmd()) // completion is stale while detail owns the foreground.
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m, cmd = updateModel(m, cmd())
	m = loadAttention(m, cmd)
	require.Equal(t, viewAttention, m.view)
	require.False(t, m.attention.clearing)
}

func TestAttentionClearCompletionSurvivesReturnFetch(t *testing.T) {
	m, a := attentionFixture(t)
	m.layout = splitlayout.Stacked
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, clearCmd := updateModel(m, keyRune('c'))
	require.True(t, m.attention.clearing)

	// Return to Attention and let its fresh read finish before the write.
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, viewDetail, m.view)
	require.True(t, m.attention.returnToAttention)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.NotNil(t, cmd, "stacked detail back should dispatch the pop event")
	m, cmd = updateModel(m, cmd())
	require.Equal(t, viewAttention, m.view)
	m = loadAttention(m, cmd)
	require.Len(t, m.attention.rows, 2)

	// The daemon commits the clear after the return read captured the old row.
	a.rows = a.rows[1:]
	m, cmd = updateModel(m, clearCmd())
	require.False(t, m.attention.clearing)
	require.NotNil(t, cmd, "a successful hidden clear must refresh the visible Attention view")
	m = loadAttention(m, cmd)

	require.Len(t, m.attention.rows, 1)
	require.Equal(t, a.rows[0].issue.UID, m.attention.rows[0].issue.UID)
}

func TestAttentionReturnContextReceivesMutationFailure(t *testing.T) {
	m, _ := attentionFixture(t)
	m.connGen = 2
	issue := testIssue("issue1")
	m.view = viewDetail
	m.detail = detailModel{issue: &issue, gen: 7, scopePID: issue.ProjectID}
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)

	failed := mutationDoneMsg{
		connGen: m.connGen,
		origin:  "detail",
		gen:     m.detail.gen,
		kind:    "reopen",
		err:     errors.New("permission denied"),
	}
	stale := failed
	stale.connGen = m.connGen - 1
	m, _ = updateModel(m, stale)
	require.Empty(t, m.attention.returnContext.detail.status,
		"a completion from another connection must not update the saved detail")

	m, _ = updateModel(m, failed)
	require.Contains(t, stripANSI(m.attention.returnContext.detail.status), "reopen failed: permission denied")
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, viewDetail, m.view)
	require.Contains(t, stripANSI(m.detail.status), "reopen failed: permission denied",
		"returning from Attention must preserve a matching mutation failure")
}

func TestAttentionDetailFromEmptyScope(t *testing.T) {
	m, a := attentionFixture(t)
	m.scope = scope{}
	m.view = viewProjects
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, a.rows[0].issue.ProjectID, m.detail.scopePID)
	require.True(t, m.detail.allProjects)
}

func TestAttentionMouseReturnFlagClears(t *testing.T) {
	for _, click := range []bool{false, true} {
		m, _ := attentionFixture(t)
		m = resizeModel(m, 160, 40)
		m, cmd := m.transitionToAttention()
		m = loadAttention(m, cmd)
		m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
		require.True(t, m.attention.returnToAttention)
		if click {
			m, _ = m.mouseLeftClick(0, 4)
		} else {
			m, _ = m.mouseWheelAt(1, 0)
		}
		require.Equal(t, focusList, m.focus)
		require.False(t, m.attention.returnToAttention)
	}
}

// Opening Attention must expose the ordinary detail actions and preserve the
// windowed list on either back key, in both supported layouts.
func TestAttentionDetailActionsAndScrollReturn(t *testing.T) {
	for _, width := range []int{80, 160} {
		for _, back := range []rune{tea.KeyEscape, tea.KeyBackspace} {
			t.Run(fmt.Sprintf("%d/%d", width, back), func(t *testing.T) {
				m, a := attentionFixture(t)
				m = resizeModel(m, width, 24)
				for i := range 50 {
					a.rows = append(a.rows, attentionRequest{issue: testIssue(fmt.Sprintf("row%d", i)), from: "sam", message: "Review"})
				}
				m, cmd := m.transitionToAttention()
				m = loadAttention(m, cmd)
				for range 30 {
					m = sendRune(m, 'j')
				}
				before := renderAttention(m)
				uid := m.attention.selectedUID
				m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
				require.Equal(t, viewDetail, m.view)
				require.True(t, m.detailIsActive())
				require.Equal(t, uid, m.detail.issue.UID)
				for _, action := range []struct {
					key  rune
					kind inputKind
				}{{'e', inputBodyEditForm}, {'c', inputCommentForm}} {
					opened := sendRune(m, action.key)
					require.Equal(t, action.kind, opened.input.kind)
				}
				m, cmd = updateModel(m, tea.KeyPressMsg{Code: back})
				if m.view == viewDetail {
					require.NotNil(t, cmd)
					m, cmd = updateModel(m, cmd())
				}
				require.Equal(t, viewAttention, m.view)
				m = loadAttention(m, cmd)
				require.Equal(t, uid, m.attention.selectedUID)
				require.Equal(t, 30, m.attention.cursor)
				require.Equal(t, before, renderAttention(m))
			})
		}
	}
}

func TestAttentionMessagePaging(t *testing.T) {
	m, a := attentionFixture(t)
	m = resizeModel(m, 80, 24)
	var lines []string
	for i := range 18 {
		lines = append(lines, fmt.Sprintf("Request instruction %02d.", i))
	}
	a.rows[0].message = strings.Join(lines, "\n")
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	selected := m.attention.selectedUID
	seen := stripANSI(m.View().Content)
	require.Contains(t, seen, lines[0])
	require.NotContains(t, seen, lines[17])
	for range 18 {
		m = sendKey(m, tea.KeyPgDown)
		seen += stripANSI(m.View().Content)
		require.Equal(t, selected, m.attention.selectedUID)
	}
	for _, line := range lines {
		require.Contains(t, seen, line, "paging must expose every instruction")
	}
	require.Contains(t, stripANSI(m.View().Content), lines[17])
	m = sendKey(m, tea.KeyPgUp)
	require.NotContains(t, stripANSI(m.View().Content), lines[17], "paging must clamp at the end")
	for range 18 {
		m = sendKey(m, tea.KeyPgUp)
	}
	require.Contains(t, stripANSI(m.View().Content), lines[0])
	m = sendKey(m, tea.KeyPgDown)
	beforeDetail := stripANSI(m.View().Content)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	m, cmd = updateModel(m, cmd())
	m = loadAttention(m, cmd)
	require.Equal(t, beforeDetail, stripANSI(m.View().Content), "returning from detail retains the message page")
	m = sendRune(m, 'j')
	require.Contains(t, stripANSI(m.View().Content), "Deadline reached")
	m = sendRune(m, 'k')
	require.Contains(t, stripANSI(m.View().Content), lines[0], "a new selection starts at the top")
}

func TestAttentionMessagePagingAfterReplacement(t *testing.T) {
	m, a := attentionFixture(t)
	m = resizeModel(m, 80, 24)
	a.rows[0].message = "Original request starts here.\n" + strings.Repeat("Original context line.\n", 18)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m = sendKey(m, tea.KeyPgDown)
	require.NotContains(t, stripANSI(m.View().Content), "Original request starts here.")
	a.rows = append([]attentionRequest(nil), a.rows...)
	a.rows[0].message = "Replacement request starts here.\n" + strings.Repeat("New context line.\n", 18)
	m, cmd = updateModel(m, keyRune('r'))
	m = loadAttention(m, cmd)
	require.Contains(t, stripANSI(m.View().Content), "Replacement request starts here.")
}
