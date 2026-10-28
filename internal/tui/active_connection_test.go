package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
)

// A11: the existing current-authority read supplies every view's connection
// header without opening token inventory or accepting an old connection reply.
func TestActiveConnectionHeaderEveryView(t *testing.T) {
	for _, view := range []viewID{viewList, viewDetail, viewHelp, viewEmpty, viewProjects, viewDaemons, viewFederation, viewCredentials} {
		t.Run(fmt.Sprint(view), func(t *testing.T) {
			m := newTestModel()
			m.width = 100
			m.height = 24
			m.view = view
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "token", Actor: "member", TokenAuditRead: false}})
			healthy, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.NotContains(t, healthy, "Hub:")
			require.False(t, m.tokenAuditRead)
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
	require.NotContains(t, stripANSI(m.viewContent()), "Hub:")
	m.sseStatus = sseReconnecting
	require.Contains(t, stripANSI(m.viewContent()), "Reconnecting")
	m.sseStatus = sseConnected
	m.connGen = 2
	m.authCapabilitiesReady = false
	m.activeDaemon = daemonTarget{Name: "other-hub"}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Actor: "old-reply"}})
	require.Equal(t, "member-one", m.activeAuth.Actor)
	require.Contains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, auth: AuthInfo{Kind: "token", Actor: "member-two"}})
	require.NotContains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, err: errors.New("authentication required")})
	require.Contains(t, stripANSI(m.viewContent()), "Authentication required")
}

// Status and project remain readable when long hub and account labels compete.
func TestActiveConnectionHeaderLongLabelsPreserveStatus(t *testing.T) {
	m := newTestModel()
	m.width = 80
	m.height = 24
	m.scope.projectName = "example-project"
	m.activeDaemon = daemonTarget{Name: strings.Repeat("long-hub-", 12)}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{
		Kind: "db_token", Actor: strings.Repeat("long-actor-", 12),
		Scope: &api.TokenScopeOut{
			Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
		},
		AllowedActions: []string{"issue.read"},
	}})
	m.sseStatus = sseConnected
	header, _, _ := strings.Cut(m.viewContent(), "\n")
	require.LessOrEqual(t, ansi.StringWidth(header), m.width)
	header = stripANSI(header)
	require.Contains(t, header, "Read-only")
	require.Contains(t, header, "Project: example-project")
	require.Contains(t, header, "Hub: long-hub-")
	require.Contains(t, header, "Account: long-actor-")
	require.NotContains(t, header, m.activeDaemon.Name)
	require.NotContains(t, header, m.activeAuth.Actor)
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
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "db_token", Actor: "member"}})
		m.sseStatus = sseReconnecting
		return m
	}
	var rows []string
	for _, tc := range []struct {
		width int
		hub   string
	}{
		{80, strings.Repeat("long-hub-", 12)},
		{100, strings.Repeat("long-hub-", 12)},
		{160, "shared-hub"},
	} {
		header, _, _ := strings.Cut(model(tc.width, tc.hub).viewContent(), "\n")
		require.LessOrEqual(t, ansi.StringWidth(header), tc.width)
		header = stripANSI(header)
		require.Contains(t, header, "Reconnecting")
		require.Contains(t, header, "Project: example-project", "width %d", tc.width)
		rows = append(rows, header)
	}
	require.Contains(t, rows[2], "Daemon: shared-hub · kata カタ · v0.1.0")
	assertGolden(t, "active-connection-header", strings.Join(rows, "\n"))
}

// A healthy connection must leave the title bar exactly as it renders
// without connection authority.
func TestActiveConnectionHeaderHiddenWhenConnected(t *testing.T) {
	m := newTestModel()
	m.width = 100
	m.height = 24
	m.activeDaemon = daemonTarget{Name: "shared-hub"}
	want, _, _ := strings.Cut(m.viewContent(), "\n")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "token", Actor: "member"}})
	got, _, _ := strings.Cut(m.viewContent(), "\n")
	require.Equal(t, want, got)
}

// Attention states clear the account until a read-only connection is ready.
func TestActiveConnectionHeaderAttentionStates(t *testing.T) {
	for _, tc := range []struct {
		status   string
		authKind string
		ready    bool
		sse      sseConnState
		err      string
	}{
		{status: "Authentication required", err: "authentication required"},
		{status: "Connection unavailable", err: "service unavailable"},
		{status: "Connecting"},
		{status: "Reconnecting", authKind: "insecure_readonly", ready: true, sse: sseReconnecting},
		{status: "Disconnected", authKind: "bootstrap", ready: true, sse: sseDisconnected},
	} {
		t.Run(tc.status, func(t *testing.T) {
			m := newTestModel()
			m.width = 160
			m.height = 24
			m.scope.projectName = "example-project"
			m.activeDaemon = daemonTarget{Name: "shared-hub"}
			m.authCapabilitiesRequired = true
			m.authCapabilitiesReady = tc.ready
			m.activeAuth = AuthInfo{Kind: "token", Actor: "member"}
			if tc.authKind != "" {
				m.activeAuth.Kind = tc.authKind
			}
			m.activeAuthError = tc.err
			m.sseStatus = tc.sse
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.True(t, strings.HasPrefix(header, tc.status+" · Hub: shared-hub · Account: — · Project: example-project"), "header: %s", header)
			require.NotContains(t, header, "member")
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
			m.sseStatus = sseConnected
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.True(t, strings.HasPrefix(header, "Read-only · Hub: Local daemon · Account: "+tc.account+" · "), "header: %s", header)
		})
	}
}

func TestActiveConnectionHeaderScopedReadOnlyAccount(t *testing.T) {
	m := newTestModel()
	m.width = 160
	m.height = 24
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{
		Kind: "db_token", Actor: "member", Scope: &api.TokenScopeOut{
			Kind: "issue_subtree", ProjectUID: "01HZNQ7VFPK1XGD8R5MABCD4EX", RootIssueUID: "01HZNQ7VFPK1XGD8R5MABCD5YZ",
		},
		AllowedActions: []string{"issue.read"},
	}})
	m.sseStatus = sseConnected
	header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
	require.Contains(t, header, "Read-only · Hub: Local daemon · Account: member · ")
}

// Expiry can precede its scheduled update while authority still looks ready.
func TestActiveConnectionHeaderExpiredBeforeTickHidesAccount(t *testing.T) {
	m := newTestModel()
	m.width = 160
	m.height = 24
	past := time.Now().Add(-time.Minute)
	m.authCapabilitiesRequired = true
	m.authCapabilitiesReady = true
	m.sseStatus = sseConnected
	m.activeAuth = AuthInfo{Kind: "db_token", Actor: "expired-member", ExpiresAt: &past}
	header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
	require.Contains(t, header, "Credential expired · Hub: Local daemon · Account: — · ")
	require.NotContains(t, header, "expired-member")
}
