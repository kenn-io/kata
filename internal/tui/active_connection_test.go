package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// Every view shows the connection status when it needs attention.
func TestActiveConnectionHeaderEveryView(t *testing.T) {
	for _, view := range []viewID{viewList, viewDetail, viewHelp, viewEmpty, viewProjects, viewDaemons, viewFederation, viewCredentials} {
		t.Run(fmt.Sprint(view), func(t *testing.T) {
			m := newTestModel()
			m.width = 100
			m.height = 24
			m.view = view
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "token", Actor: "member", TokenAuditRead: false}})
			m.sseStatus = sseReconnecting
			rows := strings.Split(stripANSI(m.viewContent()), "\n")
			header := rows[1]
			if strings.Contains(rows[0], "Daemon: shared-hub") {
				require.NotContains(t, header, "Hub: shared-hub")
			} else {
				require.Contains(t, header, "Hub: shared-hub")
			}
			require.Contains(t, header, "Reconnecting")
			m.width = 60
			narrow := connectionAttentionRow(t, m)
			require.LessOrEqual(t, ansi.StringWidth(narrow), 60)
			require.Contains(t, stripANSI(narrow), "Reconnecting")
		})
	}
}

func TestActiveConnectionHeaderFencesStaleReplies(t *testing.T) {
	m := newTestModel()
	m.width = 100
	m.height = 24
	m.connGen = 1
	expiry := time.Now().Add(time.Hour)
	m, cmd := m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Kind: "token", Actor: "member-one", ExpiresAt: &expiry}})
	require.NotNil(t, cmd, "finite credential expiry must schedule a UI update")
	m.connGen = 2
	m.authCapabilitiesReady = false
	m.activeAuth = AuthInfo{}
	m.activeDaemon = daemonTarget{Name: "other-hub"}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Actor: "old-reply"}})
	require.Empty(t, m.activeAuth.Actor)
	require.Contains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, auth: AuthInfo{Kind: "token", Actor: "member-two"}})
	require.True(t, m.authCapabilitiesReady)
	require.Equal(t, "member-two", m.activeAuth.Actor)
	require.NotContains(t, stripANSI(m.viewContent()), "Hub:")
}

// The status row keeps only context that the adjacent tab header truncates.
func TestActiveConnectionHeaderDoesNotRepeatVisibleTitleContext(t *testing.T) {
	priorVersion := kataVersion
	kataVersion = "v0.1.0"
	t.Cleanup(func() { kataVersion = priorVersion })
	m := newTestModel()
	m.width = 240
	m.height = 24
	m.scope = scope{projectID: 7, projectName: "example-project"}
	m.activeDaemon = daemonTarget{Name: "shared-hub", URL: "http://100.64.0.5:7777"}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "token", Actor: "member"}})
	m.sseStatus = sseReconnecting
	rows := strings.Split(stripANSI(m.viewContent()), "\n")
	first, status := rows[0], rows[1]
	for _, visible := range []string{"Project: example-project", "Daemon: shared-hub", "kata カタ", kataVersion} {
		require.Contains(t, first, visible)
		require.NotContains(t, status, visible)
	}
	require.NotContains(t, status, "Hub: shared-hub")
	require.Contains(t, status, "Reconnecting")
	require.Contains(t, status, "Account: —")
}

func TestActiveConnectionHeaderKeepsTruncatedTitleContext(t *testing.T) {
	priorVersion := kataVersion
	kataVersion = "v0.1.0"
	t.Cleanup(func() { kataVersion = priorVersion })
	m := newTestModel()
	m.width = 60
	m.height = 24
	m.scope = scope{projectID: 7, projectName: "p"}
	m.activeDaemon = daemonTarget{Name: "d"}
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = true
	m.activeAuth = AuthInfo{Kind: "token", Actor: "member"}
	m.sseStatus = sseDisconnected
	rows := strings.Split(stripANSI(m.viewContent()), "\n")
	first, status := rows[0], rows[1]
	for _, context := range []string{"kata カタ", kataVersion} {
		if !strings.Contains(first, context) {
			require.Contains(t, status, context)
		} else {
			require.NotContains(t, status, context)
		}
	}
	if strings.Contains(first, "Daemon: d") {
		require.NotContains(t, status, "Hub: d")
	} else {
		require.Contains(t, status, "Hub: d")
	}
	require.Contains(t, status, "Disconnected")
	require.Contains(t, status, "Account: —")
}

// A healthy connection must leave the title bar exactly as it renders
// without connection authority.
func TestActiveConnectionHeaderHiddenWhenConnected(t *testing.T) {
	for _, view := range []viewID{viewList, viewDetail, viewHelp, viewEmpty, viewProjects, viewDaemons, viewFederation, viewCredentials} {
		t.Run(fmt.Sprint(view), func(t *testing.T) {
			m := newTestModel()
			m.view = view
			m.width = 100
			m.height = 24
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			want, _, _ := strings.Cut(m.viewContent(), "\n")
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "token", Actor: "member"}})
			got, _, _ := strings.Cut(m.viewContent(), "\n")
			require.Equal(t, want, got)
		})
	}
}

// Attention states clear the account until a read-only connection is ready.
func TestActiveConnectionHeaderAttentionStates(t *testing.T) {
	for _, tc := range []struct {
		status string
		auth   AuthInfo
		absent string
		ready  bool
		sse    sseConnState
		err    string
	}{
		{status: "Authentication required", err: "authentication required"},
		{status: "Connection unavailable", err: "service unavailable"},
		{status: "Connecting"},
		{status: "Reconnecting", auth: AuthInfo{Kind: "insecure_readonly"}, absent: "Anonymous (read-only)", ready: true, sse: sseReconnecting},
		{status: "Credential expired", auth: AuthInfo{Kind: "db_token", Actor: "expired-member", ExpiresAt: new(time.Now().Add(-time.Minute))}, absent: "expired-member", ready: true},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m := newTestModel()
			m.width = 160
			m.height = 24
			m.scope.projectName = "example-project"
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			m.authCapabilitiesRequired = true
			m.authCapabilitiesReady = tc.ready
			m.activeAuth = tc.auth
			m.activeAuthError = tc.err
			m.sseStatus = tc.sse
			header := connectionAttentionRow(t, m)
			require.True(t, strings.HasPrefix(header, tc.status+" · Hub: shared-hub · Account: —") ||
				strings.HasPrefix(header, tc.status+" · Account: —"), "header: %s", header)
			require.NotContains(t, header, "Project: example-project")
			if tc.absent != "" {
				require.NotContains(t, header, tc.absent)
			}
		})
	}
}

func TestActiveConnectionHeaderReadOnlyAccount(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		account string
	}{
		{kind: "insecure_readonly", account: "Anonymous (read-only)"},
		{kind: "bootstrap", account: "Bootstrap token"},
		{kind: "trusted_proxy_absent", account: "Proxy without account"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			m := newTestModel()
			m.width = 160
			m.height = 24
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: tc.kind}})
			header := connectionAttentionRow(t, m)
			require.True(t, strings.HasPrefix(header, "Read-only · Hub: Local daemon · Account: "+tc.account) ||
				strings.HasPrefix(header, "Read-only · Account: "+tc.account), "header: %s", header)
		})
	}
}

func connectionAttentionRow(t *testing.T, m Model) string {
	t.Helper()
	rows := strings.Split(stripANSI(m.viewContent()), "\n")
	require.GreaterOrEqual(t, len(rows), 2)
	require.Contains(t, rows[0], "jobs runs workflows")
	return rows[1]
}
