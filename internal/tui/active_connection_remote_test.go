package tui

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A11: an unnamed direct remote target must not be labeled a local daemon,
// and typed authentication rejection must clear current account authority.
func TestActiveConnectionDirectRemoteAndRevoked(t *testing.T) {
	t.Run("direct_remote", func(t *testing.T) {
		m := newTestModel()
		m.width = 100
		m.height = 24
		m.activeDaemon = daemonTarget{URL: "https://daemon.example"}
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
		header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
		require.Contains(t, header, "Hub: daemon.example")
		require.NotContains(t, header, "Local daemon")
	})
	t.Run("revoked", func(t *testing.T) {
		m := newTestModel()
		m.width = 100
		m.height = 24
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{err: &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "credential revoked"}})
		header, _, _ := strings.Cut(stripANSI(m.viewContent()), "\n")
		require.Contains(t, header, "Authentication required")
		require.NotContains(t, header, "member")
	})
}
