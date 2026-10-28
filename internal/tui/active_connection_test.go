package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
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
			expireActiveCredential(&m)
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.Contains(t, header, "Hub: shared-hub")
			require.Contains(t, header, "Account: member")
			require.Contains(t, header, "Credential expired")
			m.width = 60
			narrow, _, _ := strings.Cut(m.viewContent(), "\n")
			require.LessOrEqual(t, ansi.StringWidth(narrow), 60)
			require.Contains(t, stripANSI(narrow), "Credential expired")
		})
	}
}

func TestActiveConnectionHeaderFencesStaleAccountAndExpiry(t *testing.T) {
	m := newTestModel()
	m.width = 100
	m.height = 24
	m.connGen = 1
	expiry := time.Now().Add(time.Hour)
	m, cmd := m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Kind: "token", Actor: "member-one", ExpiresAt: &expiry}})
	require.NotNil(t, cmd, "finite credential expiry must schedule a UI update")
	require.NotContains(t, stripANSI(m.viewContent()), "Account:")
	m.sseStatus = sseReconnecting
	require.Contains(t, stripANSI(m.viewContent()), "Reconnecting")
	m.sseStatus = sseConnected
	m.connGen = 2
	m.authCapabilitiesReady = false
	m.activeDaemon = daemonTarget{Name: "other-hub"}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Actor: "old-reply"}})
	require.NotContains(t, stripANSI(m.viewContent()), "member-one")
	require.NotContains(t, stripANSI(m.viewContent()), "old-reply")
	require.Contains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, auth: AuthInfo{Kind: "token", Actor: "member-two"}})
	require.NotContains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, err: errors.New("authentication required")})
	require.NotContains(t, stripANSI(m.viewContent()), "member-two")
	require.Contains(t, stripANSI(m.viewContent()), "Authentication required")
	past := time.Now().Add(-time.Minute)
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, auth: AuthInfo{Actor: "expired-member", ExpiresAt: &past}})
	require.Contains(t, stripANSI(m.viewContent()), "Credential expired")
}

// A11 narrow layouts must preserve readable status even for long account and
// configured hub names. Clipping labels must not hide expiry or reconnection.
func TestActiveConnectionHeaderLongLabelsPreserveStatus(t *testing.T) {
	m := newTestModel()
	m.width = 60
	m.height = 24
	m.activeDaemon = daemonTarget{Name: strings.Repeat("long-hub-", 12)}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: strings.Repeat("member-", 12)}})
	expireActiveCredential(&m)
	header, _, _ := strings.Cut(m.viewContent(), "\n")
	require.LessOrEqual(t, ansi.StringWidth(header), 60)
	require.Contains(t, stripANSI(header), "Credential expired")
	require.Contains(t, stripANSI(header), "Hub:")
	require.Contains(t, stripANSI(header), "Account:")
}

// Connections without an account must say how they authenticated; only an
// owner-local connection is labeled local.
func TestActiveConnectionHeaderLabelsActorlessAuthKinds(t *testing.T) {
	for kind, want := range map[string]string{
		"static_token":                    "Account: Shared daemon token",
		"bootstrap":                       "Account: Bootstrap token",
		"insecure_readonly":               "Account: Anonymous (read-only)",
		"unauthenticated_private_network": "Account: Unauthenticated private network",
		"trusted_proxy_absent":            "Account: Proxy without account",
		"none":                            "Account: Local connection",
		"":                                "Account: No account",
	} {
		t.Run(kind, func(t *testing.T) {
			m := newTestModel()
			m.width = 120
			m.height = 24
			m.activeDaemon = daemonTarget{URL: "http://100.64.0.5:7777"}
			m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: kind}})
			expireActiveCredential(&m)
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.Contains(t, header, want)
		})
	}
}

// The connection summary shares the title-bar row, so it must yield to the
// project scope label at narrow widths and leave the title bar's right
// cluster visible when the row has room.
func TestActiveConnectionHeaderKeepsTitleBarContent(t *testing.T) {
	defer snapshotInit(t)()
	priorVersion := kataVersion
	kataVersion = "v0.1.0"
	t.Cleanup(func() { kataVersion = priorVersion })
	model := func(width int, hub, account string) Model {
		m := newTestModel()
		m.width = width
		m.height = 24
		m.scope = scope{projectID: 7, projectName: "example-project"}
		m.activeDaemon = daemonTarget{Name: hub, URL: "http://100.64.0.5:7777"}
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Kind: "db_token", Actor: account}})
		expireActiveCredential(&m)
		return m
	}
	var rows []string
	for _, tc := range []struct {
		width        int
		hub, account string
	}{
		{80, strings.Repeat("long-hub-", 12), strings.Repeat("member-", 12)},
		{100, strings.Repeat("long-hub-", 12), strings.Repeat("member-", 12)},
		{160, "shared-hub", "member"},
	} {
		header, _, _ := strings.Cut(model(tc.width, tc.hub, tc.account).viewContent(), "\n")
		require.LessOrEqual(t, ansi.StringWidth(header), tc.width)
		header = stripANSI(header)
		require.Contains(t, header, "Credential expired")
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
	require.NotContains(t, stripANSI(got), "Account:")
}

// expireActiveCredential simulates the expiry tick passing on a healthy
// connection, the one attention state that still names the account.
func expireActiveCredential(m *Model) {
	past := time.Now().Add(-time.Minute)
	m.activeAuth.ExpiresAt = &past
}
