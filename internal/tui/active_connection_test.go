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
			before := m.viewContent()
			header, _, _ := strings.Cut(stripANSI(before), "\n")
			require.Contains(t, header, "Hub: shared-hub")
			require.Contains(t, header, "Account: member")
			require.Contains(t, header, "Connected")
			require.False(t, m.tokenAuditRead)
			m.width = 60
			narrow, _, _ := strings.Cut(m.viewContent(), "\n")
			require.LessOrEqual(t, ansi.StringWidth(narrow), 60)
			require.Contains(t, stripANSI(narrow), "Connected")
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
	require.Contains(t, stripANSI(m.viewContent()), "Account: member-one")
	m.connGen = 2
	m.authCapabilitiesReady = false
	m.activeDaemon = daemonTarget{Name: "other-hub"}
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 1, auth: AuthInfo{Actor: "old-reply"}})
	require.NotContains(t, stripANSI(m.viewContent()), "member-one")
	require.NotContains(t, stripANSI(m.viewContent()), "old-reply")
	require.Contains(t, stripANSI(m.viewContent()), "Connecting")
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{connGen: 2, auth: AuthInfo{Kind: "token", Actor: "member-two"}})
	require.Contains(t, stripANSI(m.viewContent()), "Account: member-two")
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
	header, _, _ := strings.Cut(m.viewContent(), "\n")
	require.LessOrEqual(t, ansi.StringWidth(header), 60)
	require.Contains(t, stripANSI(header), "Connected")
	require.Contains(t, stripANSI(header), "Hub:")
	require.Contains(t, stripANSI(header), "Account:")
}
