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
			m.sseStatus = sseReconnecting
			header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
			require.Contains(t, header, "Hub: shared-hub")
			require.Contains(t, header, "Account: —")
			require.NotContains(t, header, "Account: member")
			require.Contains(t, header, "Reconnecting")
			m.width = 60
			narrow, _, _ := strings.Cut(m.viewContent(), "\n")
			require.LessOrEqual(t, ansi.StringWidth(narrow), 60)
			require.Contains(t, stripANSI(narrow), "Reconnecting")
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
	require.Contains(t, stripANSI(m.viewContent()), "Account: —")
	require.NotContains(t, stripANSI(m.viewContent()), "expired-member")
}

// A11 narrow layouts must preserve readable status for long configured hub
// names. Clipping labels must not hide reconnection.
func TestActiveConnectionHeaderLongLabelsPreserveStatus(t *testing.T) {
	m := newTestModel()
	m.width = 60
	m.height = 24
	m.activeDaemon = daemonTarget{Name: strings.Repeat("long-hub-", 12)}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
	m.sseStatus = sseReconnecting
	header, _, _ := strings.Cut(m.viewContent(), "\n")
	require.LessOrEqual(t, ansi.StringWidth(header), 60)
	require.Contains(t, stripANSI(header), "Reconnecting")
	require.Contains(t, stripANSI(header), "Hub:")
	require.Contains(t, stripANSI(header), "Account: —")
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
	require.NotContains(t, stripANSI(got), "Account:")
}
