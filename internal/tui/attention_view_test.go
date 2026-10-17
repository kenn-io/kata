package tui

import (
	"context"
	"encoding/json/jsontext"
	"errors"
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
		t.Run(string(rune('a'+origin)), func(t *testing.T) {
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
