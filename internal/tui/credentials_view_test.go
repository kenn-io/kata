package tui

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/tui/helplayout"
)

func TestCredentialsViewCapabilityGateDoesNotFetchWhenUnavailable(t *testing.T) {
	var requests atomic.Int32
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/tokens": func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			respondJSON(t, w, map[string]any{"tokens": []any{}, "observed_at": time.Now()})
		},
	})
	m := newTestModel()
	m.api = NewClient(srv.URL, srv.Client())
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: false}})

	out, cmd := updateModel(m, keyRune('C'))

	assert.Equal(t, viewCredentials, out.view)
	assert.Nil(t, cmd)
	assert.Zero(t, requests.Load())
	assert.Contains(t, stripANSI(out.viewContent()), "credential audit unavailable")
}

func TestCredentialsViewLoadsThroughUndoClient(t *testing.T) {
	var requests atomic.Int32
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/tokens": func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			respondJSON(t, w, map[string]any{"tokens": []any{}, "observed_at": time.Now()})
		},
	})
	m := newTestModel()
	m.api = newUndoClient(NewClient(srv.URL, srv.Client()))
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: true}})
	_, cmd := updateModel(m, keyRune('C'))
	require.NotNil(t, cmd)
	loaded, ok := cmd().(credentialsLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err)
	require.EqualValues(t, 1, requests.Load())
}

func TestCredentialsViewStartsLoadingWhenCapabilityDiscoveryCompletes(t *testing.T) {
	var requests atomic.Int32
	srv := mockDaemon(t, map[string]http.HandlerFunc{
		"/api/v1/tokens": func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			respondJSON(t, w, map[string]any{"tokens": []any{}, "observed_at": time.Now()})
		},
	})
	m := newTestModel()
	m.api = NewClient(srv.URL, srv.Client())
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = false

	opened, cmd := updateModel(m, keyRune('C'))

	assert.Equal(t, viewCredentials, opened.view)
	require.NotNil(t, cmd, "entering during discovery must ensure a capability request is in flight")
	assert.Contains(t, stripANSI(opened.viewContent()), "loading credential inventory")
	assert.NotContains(t, stripANSI(opened.viewContent()), "credential audit unavailable")

	ready, fetchCmd := updateModel(opened, authCapabilitiesMsg{
		connGen: opened.connGen,
		auth:    AuthInfo{TokenAuditRead: true},
	})

	assert.True(t, ready.credentials.available)
	assert.True(t, ready.credentials.loading)
	require.NotNil(t, fetchCmd)
	loaded, ok := fetchCmd().(credentialsLoadedMsg)
	require.True(t, ok)
	assert.Equal(t, ready.credentials.gen, loaded.gen)
	assert.Equal(t, int32(1), requests.Load())
}

func TestCredentialsViewReportsCapabilityDiscoveryErrorAndRetries(t *testing.T) {
	m := newTestModel()
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = false
	m, _ = updateModel(m, keyRune('C'))

	failed, cmd := updateModel(m, authCapabilitiesMsg{
		connGen: m.connGen,
		err:     errors.New("temporary capability failure"),
	})

	assert.Nil(t, cmd)
	rendered := stripANSI(failed.viewContent())
	assert.Contains(t, rendered, "failed to determine credential audit access")
	assert.Contains(t, rendered, "temporary capability failure")
	assert.NotContains(t, rendered, "does not have token audit read capability")

	retrying, retryCmd := updateModel(failed, keyRune('r'))

	assert.True(t, retrying.credentials.capabilityPending)
	assert.True(t, retrying.credentials.loading)
	assert.NoError(t, retrying.credentials.err)
	require.NotNil(t, retryCmd)
}

func TestCredentialsViewLoadsNewestFirstAndUsesServerState(t *testing.T) {
	older := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)
	newer := older.Add(24 * time.Hour)
	observed := newer.Add(time.Hour)
	expiresLater := observed.Add(24 * time.Hour)
	m := newTestModel()
	m.width, m.height = 180, 32
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: true}})
	m, _ = updateModel(m, keyRune('C'))
	gen := m.credentials.gen

	out, tick := updateModel(m, credentialsLoadedMsg{
		connGen:    m.connGen,
		gen:        gen,
		observedAt: observed,
		tokens: []TokenInfo{
			{ID: 1, Actor: "older-actor", CreatedAt: older, State: "live"},
			{ID: 2, Actor: "newer-actor", CreatedAt: newer, ExpiresAt: &expiresLater, State: "expired"},
		},
	})

	require.Len(t, out.credentials.tokens, 2)
	assert.Equal(t, int64(2), out.credentials.tokens[0].ID)
	assert.Equal(t, int64(1), out.credentials.tokens[1].ID)
	assert.Equal(t, observed, out.credentials.observedAt)
	require.NotNil(t, tick, "accepted load should arm periodic refresh")
	rendered := stripANSI(out.viewContent())
	assert.Less(t, credentialIndexOf(t, rendered, "newer-actor"), credentialIndexOf(t, rendered, "older-actor"))
	assert.Contains(t, rendered, "expired", "server state is authoritative even before expires_at")
}

func TestCredentialsViewManualAndPeriodicRefreshStayViewScoped(t *testing.T) {
	m := newTestModel()
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: true}})
	m, enterCmd := updateModel(m, keyRune('C'))
	require.NotNil(t, enterCmd)
	gen := m.credentials.gen

	_, manualCmd := updateModel(m, keyRune('r'))
	require.NotNil(t, manualCmd)
	manualMsg, ok := manualCmd().(credentialsLoadedMsg)
	require.True(t, ok)
	assert.Equal(t, gen, manualMsg.gen)

	_, periodicCmd := updateModel(m, credentialsRefreshTickMsg{connGen: m.connGen, gen: gen})
	require.NotNil(t, periodicCmd)
	periodicMsg, ok := periodicCmd().(credentialsLoadedMsg)
	require.True(t, ok)
	assert.Equal(t, gen, periodicMsg.gen)

	exited, _ := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	assert.NotEqual(t, viewCredentials, exited.view)
	assert.Greater(t, exited.credentials.gen, gen)
	_, staleTickCmd := updateModel(exited, credentialsRefreshTickMsg{connGen: exited.connGen, gen: gen})
	assert.Nil(t, staleTickCmd)
}

func TestCredentialsViewDropsResponsesAfterExitOrDaemonSwitch(t *testing.T) {
	m := newTestModel()
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: true}})
	m, _ = updateModel(m, keyRune('C'))
	gen := m.credentials.gen
	m.credentials.tokens = []TokenInfo{{ID: 7, Actor: "current"}}

	exited, _ := updateModel(m, tea.KeyPressMsg{Code: tea.KeyEscape})
	afterExit, _ := updateModel(exited, credentialsLoadedMsg{
		connGen: exited.connGen, gen: gen,
		tokens: []TokenInfo{{ID: 8, Actor: "stale-exit"}},
	})
	require.Len(t, afterExit.credentials.tokens, 1)
	assert.Equal(t, int64(7), afterExit.credentials.tokens[0].ID)

	afterSwitch, _ := updateModel(m, credentialsLoadedMsg{
		connGen: m.connGen + 1, gen: gen,
		tokens: []TokenInfo{{ID: 9, Actor: "stale-daemon"}},
	})
	require.Len(t, afterSwitch.credentials.tokens, 1)
	assert.Equal(t, int64(7), afterSwitch.credentials.tokens[0].ID)
}

func TestCredentialsViewGlobalExitPreservesUnderlyingReturnView(t *testing.T) {
	m := newTestModel()
	m.daemonTargets = []daemonTarget{{Name: "local", Local: true}}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{TokenAuditRead: true}})
	m, _ = updateModel(m, keyRune('C'))
	gen := m.credentials.gen

	out, cmd := updateModel(m, keyRune('D'))

	require.Nil(t, cmd)
	assert.Equal(t, viewDaemons, out.view)
	assert.Equal(t, viewList, out.prevView)
	assert.Greater(t, out.credentials.gen, gen)
}

func TestCredentialsViewArmsOnlyOnePeriodicTimerAcrossManualLoads(t *testing.T) {
	m := newTestModel()
	m.view = viewCredentials
	m.credentials = credentialAuditState{available: true, gen: 3}

	first, firstTick := m.handleCredentialsLoaded(credentialsLoadedMsg{gen: 3})
	require.NotNil(t, firstTick)
	_, duplicateTick := first.handleCredentialsLoaded(credentialsLoadedMsg{gen: 3})

	assert.Nil(t, duplicateTick, "manual loads must not multiply periodic refresh loops")
}

func TestCredentialsViewRendersRedactedFieldsAndSanitizesTerminalContent(t *testing.T) {
	created := time.Date(2026, 9, 15, 10, 11, 12, 0, time.UTC)
	expires := created.Add(time.Hour)
	used := created.Add(10 * time.Minute)
	revoked := created.Add(20 * time.Minute)
	name := "deploy\x1b[31m\nnext"
	m := newTestModel()
	m.view = viewCredentials
	m.width, m.height = 180, 32
	m.credentials.available = true
	m.credentials.observedAt = created.Add(30 * time.Minute)
	m.credentials.tokens = []TokenInfo{{
		ID: 12, Name: &name, Actor: "agent\u202eactor", State: "revoked",
		Scope:     &TokenScopeInfo{Kind: "issue_subtree", ProjectUID: "01PROJECT", RootIssueUID: "01ROOT"},
		CreatedAt: created, ExpiresAt: &expires, LastUsedAt: &used, RevokedAt: &revoked,
	}}

	rendered := stripANSI(m.viewContent())

	assert.Contains(t, rendered, "state revoked")
	assert.Contains(t, rendered, "ID 12")
	assert.Contains(t, rendered, "name deploy\\nnext")
	assert.Contains(t, rendered, "actor agentactor")
	assert.Contains(t, rendered, "scope issue_subtree")
	assert.Contains(t, rendered, "project 01PROJECT")
	assert.Contains(t, rendered, "root 01ROOT")
	assert.Contains(t, rendered, "created 2026-09-15 10:11:12Z")
	assert.Contains(t, rendered, "expires 2026-09-15 11:11:12Z")
	assert.Contains(t, rendered, "last observed use 2026-09-15 10:21:12Z")
	assert.Contains(t, rendered, "revoked 2026-09-15 10:31:12Z")
	assert.NotContains(t, rendered, "\x1b")
	assert.NotContains(t, rendered, "\u202e")
	assert.NotContains(t, rendered, "plaintext")
	assert.NotContains(t, rendered, "hash")
}

func TestCredentialsViewPublishesContextualFooterBindings(t *testing.T) {
	m := Model{view: viewCredentials}

	items := flattenHelpRows(m.helpRows())

	assert.Contains(t, items, helplayout.HelpItem{Key: "r", Description: "refresh"})
	assert.Contains(t, items, helplayout.HelpItem{Key: "esc", Description: "back"})
	assert.Contains(t, items, helplayout.HelpItem{Key: "D", Description: "daemons"})
}

func credentialIndexOf(t *testing.T, s, needle string) int {
	t.Helper()
	i := len(s)
	if got := strings.Index(s, needle); got >= 0 {
		i = got
	}
	require.Less(t, i, len(s), "missing %q", needle)
	return i
}
