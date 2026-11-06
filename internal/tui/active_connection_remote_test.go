package tui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A11: an unnamed direct remote target must not be labeled a local daemon,
// and typed authentication rejection must show that authentication is required.
func TestActiveConnectionDirectRemoteAndRevoked(t *testing.T) {
	t.Run("direct_remote", func(t *testing.T) {
		m := newTestModel()
		m.width = 100
		m.height = 24
		m.activeDaemon = daemonTarget{URL: "https://daemon.example"}
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
		m.sseStatus = sseReconnecting
		rows := strings.Split(stripANSI(m.viewContent()), "\n")
		require.Contains(t, rows[0], "Daemon: daemon.example")
		require.Contains(t, rows[1], "Reconnecting")
		require.NotContains(t, rows[1], "Hub: daemon.example")
		require.NotContains(t, rows[1], "Local daemon")
	})
	t.Run("revoked", func(t *testing.T) {
		m := newTestModel()
		m.width = 100
		m.height = 24
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{err: &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "credential revoked"}})
		header := connectionAttentionRow(t, m)
		require.Contains(t, header, "Authentication required")
		require.NotContains(t, header, "member")
	})
}
