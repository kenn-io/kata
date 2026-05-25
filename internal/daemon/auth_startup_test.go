package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestAuthStartupGuard_NonLoopbackTCPWithoutToken_Refuses(t *testing.T) {
	err := checkAuthStartup("100.64.0.5:7777",
		authPolicy{Token: "", InsecureReadonly: false})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-loopback TCP")
}

func TestAuthStartupGuard_NonLoopbackTCP_InsecureReadonly_Permitted(t *testing.T) {
	err := checkAuthStartup("100.64.0.5:7777",
		authPolicy{Token: "", InsecureReadonly: true})
	require.NoError(t, err)
}

func TestAuthStartupGuard_LoopbackTCP_NoTokenOK(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "[::1]:0"} {
		require.NoError(t, checkAuthStartup(addr,
			authPolicy{Token: "", InsecureReadonly: false}))
	}
}

func TestAuthStartupGuard_UnixSocket_NoTokenOK(t *testing.T) {
	require.NoError(t, checkAuthStartup("",
		authPolicy{Token: "", InsecureReadonly: false}))
}

// TestAuthStartupGuard_TokenConfigured_LoopbackOnly verifies the post-fix
// policy: a configured token is sufficient for Unix-socket and loopback TCP
// binds (where the token never reaches the wire) but is NOT sufficient for
// non-loopback TCP, where the token would travel in cleartext.
func TestAuthStartupGuard_TokenConfigured_LoopbackOnly(t *testing.T) {
	p := authPolicy{Token: "tok"}
	require.NoError(t, checkAuthStartup("", p))
	require.NoError(t, checkAuthStartup("127.0.0.1:0", p))
}

// TestAuthStartupGuard_TokenOnNonLoopback_Refuses pins the security fix:
// even with a token configured, plaintext non-loopback binds are rejected
// at startup. Operators must front the daemon with TLS termination or
// tunnel via SSH to reach it from another host.
func TestAuthStartupGuard_TokenOnNonLoopback_Refuses(t *testing.T) {
	for _, addr := range []string{"100.64.0.5:7777", ":7777", "0.0.0.0:7777", "[::]:7777"} {
		t.Run(addr, func(t *testing.T) {
			err := checkAuthStartup(addr, authPolicy{Token: "tok"})
			require.Error(t, err, "non-loopback bind %q with token must be refused", addr)
			assert.Contains(t, err.Error(), "plaintext HTTP")
		})
	}
}

func TestAuthStartupGuard_TokenOnNonLoopback_TrustPrivateNetwork_Permitted(t *testing.T) {
	p := authPolicy{Token: "tok", TrustPrivateNetwork: true}
	require.NoError(t, checkAuthStartup("100.64.0.5:7777", p))
	require.NoError(t, checkAuthStartup("192.168.1.20:7777", p))
}

func TestAuthStartupGuard_TrustPrivateNetworkWithoutToken_StillRefuses(t *testing.T) {
	err := checkAuthStartup("100.64.0.5:7777",
		authPolicy{TrustPrivateNetwork: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-loopback TCP")
}

func TestTrustPrivateNetworkWarning(t *testing.T) {
	msg, ok := TrustPrivateNetworkWarning("100.64.0.5:7777",
		config.AuthConfig{Token: "tok", TrustPrivateNetwork: true})
	require.True(t, ok)
	assert.Contains(t, msg, "WARNING")
	assert.Contains(t, msg, "non-loopback")
	assert.Contains(t, msg, "private-network confidentiality")
}

func TestTrustPrivateNetworkWarning_OnlyWhenEffective(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listen string
		auth   config.AuthConfig
	}{
		{name: "loopback", listen: "127.0.0.1:7777", auth: config.AuthConfig{Token: "tok", TrustPrivateNetwork: true}},
		{name: "no token", listen: "100.64.0.5:7777", auth: config.AuthConfig{TrustPrivateNetwork: true}},
		{name: "no trust", listen: "100.64.0.5:7777", auth: config.AuthConfig{Token: "tok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := TrustPrivateNetworkWarning(tc.listen, tc.auth)
			assert.False(t, ok)
		})
	}
}

// TestAuthStartupGuard_TokenOnNonLoopback_InsecureReadonly_StillRefuses
// covers the corner case where the operator combines a token with
// --insecure-readonly on a non-loopback bind. The token would still leak
// over the wire on every client request, so the guard refuses regardless
// of the readonly flag — the operator must drop the token (or move to
// loopback) explicitly.
func TestAuthStartupGuard_TokenOnNonLoopback_InsecureReadonly_StillRefuses(t *testing.T) {
	err := checkAuthStartup("100.64.0.5:7777",
		authPolicy{Token: "tok", InsecureReadonly: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext HTTP")
}

// TestAuthStartupGuard_WildcardBindWithoutToken_Refuses covers the listen
// shapes that bind every interface in Go's net.Listen — :port (empty host),
// 0.0.0.0:port, and [::]:port. Each is reachable from anywhere on the
// network and so must require a token unless --insecure-readonly is set.
func TestAuthStartupGuard_WildcardBindWithoutToken_Refuses(t *testing.T) {
	for _, addr := range []string{":7777", "0.0.0.0:7777", "[::]:7777"} {
		t.Run(addr, func(t *testing.T) {
			err := checkAuthStartup(addr,
				authPolicy{Token: "", InsecureReadonly: false})
			require.Error(t, err, "wildcard bind %q must require auth", addr)
			assert.Contains(t, err.Error(), "non-loopback TCP")
		})
	}
}

// TestAuthStartupGuard_UnknownHostnameWithoutToken_Refuses covers the
// conservative-default case: any non-IP, non-localhost hostname could be
// public, and the guard cannot resolve DNS, so it must require a token.
func TestAuthStartupGuard_UnknownHostnameWithoutToken_Refuses(t *testing.T) {
	err := checkAuthStartup("example.internal:7777",
		authPolicy{Token: "", InsecureReadonly: false})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-loopback TCP")
}

// TestAuthStartupGuard_LocalhostHostname_NoTokenOK covers the symbolic
// loopback name. "localhost" stays in the no-token-OK set because Go's
// resolver maps it to a loopback IP on every supported platform.
func TestAuthStartupGuard_LocalhostHostname_NoTokenOK(t *testing.T) {
	require.NoError(t, checkAuthStartup("localhost:7777",
		authPolicy{Token: "", InsecureReadonly: false}))
}

func TestServerConfig_AuthPolicyThreaded(t *testing.T) {
	cfg := ServerConfig{
		Auth:             config.AuthConfig{Token: "tok-123", TrustPrivateNetwork: true},
		InsecureReadonly: false,
	}
	got := cfg.authPolicy()
	assert.Equal(t, "tok-123", got.Token)
	assert.True(t, got.TrustPrivateNetwork)
	assert.False(t, got.InsecureReadonly)
}
