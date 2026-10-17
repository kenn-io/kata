package tui

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/notification"
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
func TestAttentionOpenIsSynchronous(t *testing.T) {
	m, _ := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, _ = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, viewDetail, m.view)
	m, cmd = updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.view == viewDetail {
		m, _ = updateModel(m, cmd())
	}
	require.Equal(t, viewAttention, m.view)
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
func TestAttentionEntryFencesDetailFollow(t *testing.T) {
	m, _ := attentionFixture(t)
	m = resizeModel(m, 160, 40)
	m.detail.issue = new(testIssue("abc4"))
	m.nextDetailFollowGen = 4
	m, _ = m.transitionToAttention()
	_, cmd := m.handleDetailFollowTick(detailFollowTickMsg{gen: 4})
	require.Nil(t, cmd)
	// Split pane focus must not steal esc from the auxiliary view.
	m.focus = focusDetail
	m = sendKey(m, tea.KeyEscape)
	require.Equal(t, viewList, m.view)
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
func TestAttentionClearStaleCompletion(t *testing.T) {
	m, _ := attentionFixture(t)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m.connGen = 3
	m.attention.gen = 7
	m.attention.clearing = true
	for _, msg := range []attentionClearedMsg{{connGen: 2, gen: 7}, {connGen: 3, gen: 6}} {
		next, cmd := m.handleAttentionCleared(msg)
		require.Nil(t, cmd)
		require.True(t, next.attention.clearing)
		require.Equal(t, m.attention.rows, next.attention.rows)
	}
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

func TestAttentionClearWrappedNilAPIError(t *testing.T) {
	m, a := attentionFixture(t)
	var apiErr *APIError
	a.clearErr = fmt.Errorf("request failed: %w", apiErr)
	m, cmd := m.transitionToAttention()
	m = loadAttention(m, cmd)
	m, cmd = updateModel(m, keyRune('c'))
	require.NotPanics(t, func() { m, _ = updateModel(m, cmd()) })
	require.NotNil(t, m.toast)
	require.Len(t, m.attention.rows, 2)
	require.False(t, m.attention.clearing)
}
