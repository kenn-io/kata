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

// Transport failures must clear stale authority even when an error chain
// contains a typed nil API error instead of a concrete HTTP status.
func TestAuthCapabilitiesWrappedNilAPIErrorFailsClosed(t *testing.T) {
	m := newTestModel()
	m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{auth: AuthInfo{Actor: "member"}})
	require.NotPanics(t, func() {
		m, _ = m.handleAuthCapabilities(authCapabilitiesMsg{err: wrappedNilCapabilityError{}})
	})
	require.False(t, m.authCapabilitiesReady)
	require.Empty(t, m.activeAuth.Actor)
	require.False(t, m.activeAuthRejected)
	require.Contains(t, m.activeAuthError, "transport unavailable")
}

type wrappedNilCapabilityError struct{}

func (wrappedNilCapabilityError) Error() string { return "transport unavailable" }
func (wrappedNilCapabilityError) Unwrap() error { return (*APIError)(nil) }
