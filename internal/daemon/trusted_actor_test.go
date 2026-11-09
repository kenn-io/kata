package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestListenerTrusted(t *testing.T) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", "100.64.0.5:7777")
	if err != nil {
		t.Fatal(err)
	}
	unixAddr := &net.UnixAddr{Name: "/run/kata/proxy.sock", Net: "unix"}
	otherTCP, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9999")

	cases := []struct {
		name      string
		local     net.Addr
		allowlist []string
		want      bool
	}{
		{"empty allowlist", tcpAddr, nil, false},
		{"nil addr", nil, []string{"100.64.0.5:7777"}, false},
		{"tcp match", tcpAddr, []string{"100.64.0.5:7777"}, true},
		{"tcp no match", otherTCP, []string{"100.64.0.5:7777"}, false},
		{"unix match with prefix", unixAddr, []string{"unix:///run/kata/proxy.sock"}, true},
		{"unix match plain path", unixAddr, []string{"/run/kata/proxy.sock"}, true},
		{"unix no match", unixAddr, []string{"/different/path"}, false},
		{"whitespace in entry trimmed", tcpAddr, []string{"  100.64.0.5:7777 "}, true},
		{"wildcard 0.0.0.0 never matches", tcpAddr, []string{"0.0.0.0:7777"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listenerTrusted(tc.local, tc.allowlist)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestWithTrustedProxyActor_Matrix(t *testing.T) {
	tcpAddr, _ := net.ResolveTCPAddr("tcp", "100.64.0.5:7777")
	otherTCP, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:9999")

	type want struct {
		kind  PrincipalKind // zero value = principal unchanged
		actor string
	}
	cases := []struct {
		name     string
		header   string
		local    net.Addr
		auth     config.AuthConfig
		incoming Principal
		want     want
	}{
		{
			name:   "mode off, no header set",
			header: "", local: tcpAddr,
			auth:     config.AuthConfig{},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "alice"},
			want:     want{kind: PrincipalDBToken, actor: "alice"},
		},
		{
			name:   "mode off, header sent but ignored",
			header: "alice", local: tcpAddr,
			auth:     config.AuthConfig{Proxy: config.ProxyConfig{TrustedProxyListeners: []string{"100.64.0.5:7777"}}},
			incoming: Principal{},
			want:     want{},
		},
		{
			name:   "mode on, untrusted listener",
			header: "alice", local: otherTCP,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "alice"},
			want:     want{kind: PrincipalDBToken, actor: "alice"},
		},
		{
			name:   "mode on, trusted listener, header set -> overwrite",
			header: "alice", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			incoming: Principal{Kind: PrincipalDBToken, Actor: "token-bob"},
			want:     want{kind: PrincipalTrustedProxy, actor: "alice"},
		},
		{
			name:   "mode on, trusted listener, header missing -> absent",
			header: "", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxyAbsent},
		},
		{
			name:   "mode on, trusted listener, whitespace-only header -> absent",
			header: "   ", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxyAbsent},
		},
		{
			name:   "mode on, trusted listener, header trimmed before storing",
			header: "  bob  ", local: tcpAddr,
			auth: config.AuthConfig{Proxy: config.ProxyConfig{
				TrustedActorHeader:    "X-Kata-Actor",
				TrustedProxyListeners: []string{"100.64.0.5:7777"},
			}},
			want: want{kind: PrincipalTrustedProxy, actor: "bob"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got Principal
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if p, ok := PrincipalFromContext(r.Context()); ok {
					got = p
				}
				w.WriteHeader(http.StatusOK)
			})
			h := withTrustedProxyActor(ServerConfig{Auth: tc.auth})(next)

			req := httptest.NewRequestWithContext(t.Context(), "POST", "/x", nil)
			if tc.header != "" {
				req.Header.Set("X-Kata-Actor", tc.header)
			}
			ctx := context.WithValue(req.Context(), http.LocalAddrContextKey, tc.local)
			if tc.incoming.Kind != "" {
				ctx = WithPrincipal(ctx, tc.incoming)
			}
			req = req.WithContext(ctx)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			assert.Equal(t, tc.want.kind, got.Kind)
			assert.Equal(t, tc.want.actor, got.Actor)
		})
	}
}
