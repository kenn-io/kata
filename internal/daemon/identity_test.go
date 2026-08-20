package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/api"
)

// TestPrincipalKind_TrustedProxyConstants locks the values of the two
// trusted-proxy principal kinds. They must be distinct from each other
// and from the kinds defined by the identity layer so logs and audit
// reads can tell them apart.
func TestPrincipalKind_TrustedProxyConstants(t *testing.T) {
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalTrustedProxyAbsent)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalDBToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalStaticToken)
	assert.NotEqual(t, PrincipalTrustedProxy, PrincipalBootstrap)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalDBToken)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalStaticToken)
	assert.NotEqual(t, PrincipalTrustedProxyAbsent, PrincipalBootstrap)

	// PrincipalKind is a string named type. Lock the snake_case string
	// values so they match the existing convention ("db_token",
	// "bootstrap", "static_token") and stay stable for log/audit
	// consumers.
	assert.Equal(t, PrincipalKind("trusted_proxy"), PrincipalTrustedProxy)
	assert.Equal(t, PrincipalKind("trusted_proxy_absent"), PrincipalTrustedProxyAbsent)
}

// TestActorFor_TrustedProxy verifies that a PrincipalTrustedProxy principal's
// Actor field is honored as the resolved actor, overriding any
// request-supplied actor string. The trusted-proxy header value is set by
// the listener-trust middleware and must win over client-claimed actors.
func TestActorFor_TrustedProxy(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	got := actorFor(ctx, "client-claim")
	assert.Equal(t, "proxy-user", got, "trusted-proxy principal wins over supplied actor")
}

// TestEnsureAttributedWriteAllowed_TrustedProxyAbsent locks the rejection
// contract for trusted-listener requests that did not carry the configured
// actor header. The error envelope must be 400 actor_header_required so
// clients can distinguish "trusted but unattributed" from generic 403s.
func TestEnsureAttributedWriteAllowed_TrustedProxyAbsent(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind: PrincipalTrustedProxyAbsent,
	})
	err := ensureAttributedWriteAllowed(ctx)
	require.Error(t, err)

	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 400, apiErr.Status)
	assert.Equal(t, "actor_header_required", apiErr.Code)
}

// TestEnsureAttributedWriteAllowed_TrustedProxyAllowed confirms the positive
// path: when the trusted-proxy middleware sets a PrincipalTrustedProxy with
// an Actor value, attributed writes proceed.
func TestEnsureAttributedWriteAllowed_TrustedProxyAllowed(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{
		Kind:  PrincipalTrustedProxy,
		Actor: "proxy-user",
	})
	require.NoError(t, ensureAttributedWriteAllowed(ctx),
		"PrincipalTrustedProxy must be allowed to do attributed writes")
}

func TestPrincipalWebLocalAllowsOrdinaryWritesButNotTokenAdministration(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Kind: PrincipalWebLocal})
	require.NoError(t, ensureAttributedWriteAllowed(ctx))

	err := ensureTokenAdminAllowed(ctx)
	require.Error(t, err)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 403, apiErr.Status)
	assert.Equal(t, "token_admin_forbidden", apiErr.Code)
}

func TestTUIBypassAllowed_RequiresOwnerLocalTransport(t *testing.T) {
	tests := []struct {
		name       string
		addr       net.Addr
		remoteAddr string
		headers    map[string]string
		want       bool
	}{
		{
			name: "unix socket",
			addr: &net.UnixAddr{Name: "/run/user/1000/kata.sock", Net: "unix"},
			want: true,
		},
		{
			name:    "forwarded unix socket",
			addr:    &net.UnixAddr{Name: "/run/user/1000/kata.sock", Net: "unix"},
			headers: map[string]string{"Forwarded": "for=192.0.2.10"},
			want:    false,
		},
		{
			name:       "direct loopback TCP",
			addr:       &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777},
			remoteAddr: "127.0.0.1:40123",
			want:       true,
		},
		{
			name:       "non-loopback peer on loopback TCP",
			addr:       &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777},
			remoteAddr: "192.0.2.10:40123",
			want:       false,
		},
		{
			name:       "forwarded loopback TCP",
			addr:       &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 7777},
			remoteAddr: "127.0.0.1:40123",
			headers:    map[string]string{"X-Forwarded-For": "192.0.2.10"},
			want:       false,
		},
		{
			name: "private network TCP",
			addr: &net.TCPAddr{IP: net.ParseIP("100.64.0.5"), Port: 7777},
			want: false,
		},
		{
			name: "missing transport metadata",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/actions/close", nil)
			request.RemoteAddr = tt.remoteAddr
			if tt.addr != nil {
				request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, tt.addr))
			}
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}

			var got bool
			withOwnerLocalTransport(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = tuiBypassAllowed(r.Context(), "tui", "done")
			})).ServeHTTP(httptest.NewRecorder(), request)

			assert.Equal(t, tt.want, got)
		})
	}
}
