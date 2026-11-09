package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
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
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.Contains(t, header, "Hub: shared-hub")
			require.Contains(t, header, "Reconnecting")
			m.width = 60
			narrow, _, _ := strings.Cut(m.viewContent(), "\n")
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

// The connection summary shares the title-bar row, so it must yield to the
// project scope label at narrow widths and leave the title bar's right
// cluster visible when the row has room.
func TestActiveConnectionHeaderKeepsTitleBarContent(t *testing.T) {
	defer snapshotInit(t)()
	priorVersion := kataVersion
	kataVersion = "v0.1.0"
	t.Cleanup(func() { kataVersion = priorVersion })
	model := func(width int, hub string) Model {
		m := newTestModel()
		m.width = width
		m.height = 24
		m.scope = scope{projectID: 7, projectName: "example-project"}
		m.activeDaemon = daemonTarget{Name: hub, URL: "http://100.64.0.5:7777"}
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{
			Kind:  "db_token",
			Actor: strings.Repeat("long-actor-", 12),
			Scope: &api.TokenScopeOut{
				Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
			},
			ExpiresAt:             new(time.Now().UTC().Add(time.Hour)),
			AllowedActions:        []string{"issue.read"},
			CloseRequiresEvidence: true,
		}})
		return m
	}
	var rows []string
	for _, tc := range []struct {
		width int
		hub   string
	}{
		{80, strings.Repeat("long-hub-", 12)},
		{100, strings.Repeat("long-hub-", 12)},
		{240, "shared-hub"},
	} {
		m := model(tc.width, tc.hub)
		header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
		if tc.width < 240 {
			require.NotContains(t, header, m.activeDaemon.Name)
			require.NotContains(t, header, m.activeAuth.Actor)
		}
		rows = append(rows, header)
	}
	require.Contains(t, rows[2], "Daemon: shared-hub · kata カタ · v0.1.0")
	assertGolden(t, "active-connection-header", strings.Join(rows, "\n"))
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
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.True(t, strings.HasPrefix(header, tc.status+" · Hub: shared-hub · Account: — · Project: example-project"), "header: %s", header)
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
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.True(t, strings.HasPrefix(header, "Read-only · Hub: Local daemon · Account: "+tc.account+" · "), "header: %s", header)
		})
	}
}
