package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestBrowserHostPolicy(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123"})
	require.NoError(t, err)

	tests := []struct {
		name string
		host string
		want int
	}{
		{name: "bound literal", host: "127.0.0.1:27123", want: http.StatusNoContent},
		{name: "localhost alias", host: "localhost:27123", want: http.StatusNoContent},
		{name: "missing", host: "", want: http.StatusBadRequest},
		{name: "foreign", host: "attacker.example:27123", want: http.StatusBadRequest},
		{name: "wrong port", host: "127.0.0.1:27124", want: http.StatusBadRequest},
		{name: "rebinding", host: "daemon.example:27123", want: http.StatusBadRequest},
		{name: "malformed", host: "127.0.0.1:27123:80", want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/", nil)
			req.Host = tt.host
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, req)

			assert.Equal(t, tt.want, response.Code)
		})
	}
}

func TestBrowserHostPolicyHostedOrigin(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{Kind: ListenerBrowser, Origin: "https://daemon.example"})
	require.NoError(t, err)

	for host, want := range map[string]int{
		"daemon.example":     http.StatusNoContent,
		"daemon.example:443": http.StatusBadRequest,
		"localhost":          http.StatusBadRequest,
		"attacker.example":   http.StatusBadRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "https://daemon.example/", nil)
		req.Host = host
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		assert.Equal(t, want, response.Code, host)
	}
}

func TestBrowserOriginPolicy(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{Kind: ListenerBrowser, Origin: "http://127.0.0.1:27123"})
	require.NoError(t, err)

	for name, origin := range map[string]string{
		"missing":      "",
		"cross origin": "https://attacker.example",
		"wrong port":   "http://127.0.0.1:27124",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", nil)
			req.Host = "127.0.0.1:27123"
			req.Header.Set("Origin", origin)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			assert.Equal(t, http.StatusForbidden, response.Code)
		})
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/projects", nil)
	req.Host = "127.0.0.1:27123"
	req.Header.Set("Origin", "http://127.0.0.1:27123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	assert.Equal(t, http.StatusNoContent, response.Code)
}

func TestLocalUISessionPolicyRequiresDirectLoopback(t *testing.T) {
	tests := []struct {
		name       string
		allow      bool
		host       string
		remoteAddr string
		headers    map[string]string
		want       int
	}{
		{
			name: "matching origin", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Origin": "http://127.0.0.1:27123"}, want: http.StatusNoContent,
		},
		{
			name: "absent origin", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123", want: http.StatusNoContent,
		},
		{
			name: "empty origin", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Origin": ""}, want: http.StatusNoContent,
		},
		{
			name: "foreign host", allow: true,
			host: "attacker.example:27123", remoteAddr: "127.0.0.1:40123", want: http.StatusBadRequest,
		},
		{
			name: "mismatched origin", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Origin": "https://attacker.example"}, want: http.StatusForbidden,
		},
		{
			name: "opaque origin", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Origin": "null"}, want: http.StatusForbidden,
		},
		{
			name: "local sessions disabled",
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123", want: http.StatusNotFound,
		},
		{
			name: "non-loopback peer", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "192.0.2.10:40123", want: http.StatusNotFound,
		},
		{
			name: "Forwarded header", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Forwarded": "for=192.0.2.10"}, want: http.StatusNotFound,
		},
		{
			name: "X-Forwarded-For header", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"X-Forwarded-For": "192.0.2.10"}, want: http.StatusNotFound,
		},
		{
			name: "X-Forwarded-Host header", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"X-Forwarded-Host": "daemon.example"}, want: http.StatusNotFound,
		},
		{
			name: "X-Forwarded-Proto header", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"X-Forwarded-Proto": "https"}, want: http.StatusNotFound,
		},
		{
			name: "X-Real-IP header", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"X-Real-IP": "192.0.2.10"}, want: http.StatusNotFound,
		},
		{
			name: "cross-site fetch", allow: true,
			host: "127.0.0.1:27123", remoteAddr: "127.0.0.1:40123",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, want: http.StatusForbidden,
		},
	}

	for _, kind := range []ListenerKind{ListenerBrowser, ListenerSharedTCP} {
		for _, tt := range tests {
			t.Run(string(kind)+"/"+tt.name, func(t *testing.T) {
				handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}), ListenerPolicy{
					Kind: kind, Origin: "http://127.0.0.1:27123", AllowLocalSession: tt.allow,
				})
				require.NoError(t, err)

				request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:27123/api/v1/ui/session/local", nil)
				request.Host = tt.host
				request.RemoteAddr = tt.remoteAddr
				for name, value := range tt.headers {
					request.Header.Set(name, value)
				}
				response := httptest.NewRecorder()

				handler.ServeHTTP(response, request)

				assert.Equal(t, tt.want, response.Code)
			})
		}
	}
}

func TestBrowserOriginPolicySharedTCPPreservesCLI(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{Kind: ListenerSharedTCP, Origin: "http://127.0.0.1:27123"})
	require.NoError(t, err)

	tests := []struct {
		name    string
		method  string
		path    string
		host    string
		headers map[string]string
		cookie  *http.Cookie
		want    int
	}{
		{
			name: "health request on shared daemon address",
			path: "/api/v1/health", host: "spoke-b:7777", want: http.StatusBadRequest,
		},
		{
			name:   "CLI mutation on shared daemon address",
			method: http.MethodPost, path: "/api/v1/projects", host: "spoke-b:7777", want: http.StatusBadRequest,
		},
		{
			name: "browser data route",
			path: "/api/v1/ui/snapshot", host: "spoke-b:7777", want: http.StatusBadRequest,
		},
		{
			name: "browser mutation without origin", method: http.MethodPost,
			path: "/api/v1/projects", host: "127.0.0.1:27123",
			headers: map[string]string{webSessionHeader: "tab-session"}, want: http.StatusForbidden,
		},
		{
			name: "browser session header",
			path: "/api/v1/projects", host: "spoke-b:7777",
			headers: map[string]string{"X-Kata-Web-Session": "tab-session"}, want: http.StatusBadRequest,
		},
		{
			name: "browser session cookie",
			path: "/api/v1/projects", host: "spoke-b:7777",
			cookie: &http.Cookie{
				Name: "kata_session_instance_a", Value: "cookie-session",
				Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
			},
			want: http.StatusBadRequest,
		},
		{
			name: "browser route",
			path: "/kata", host: "spoke-b:7777", want: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			request := httptest.NewRequest(method, "http://127.0.0.1:27123"+tt.path, nil)
			request.Host = tt.host
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}
			if tt.cookie != nil {
				request.AddCookie(tt.cookie)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assert.Equal(t, tt.want, response.Code)
		})
	}
}

func TestSharedTCPHostPolicyAcceptsBackendAndPublicAuthorities(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{
		Kind:             ListenerSharedTCP,
		Origin:           "https://daemon.example",
		BackendAuthority: "127.0.0.1:27123",
	})
	require.NoError(t, err)

	for host, want := range map[string]int{
		"daemon.example":       http.StatusNoContent,
		"127.0.0.1:27123":      http.StatusNoContent,
		"localhost:27123":      http.StatusNoContent,
		"untrusted.example:80": http.StatusBadRequest,
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/health", nil)
		request.Host = host
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		assert.Equal(t, want, response.Code, host)
	}
}

func TestSharedTCPHostPolicyAcceptsExplicitBackendAuthority(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{
		Kind:             ListenerSharedTCP,
		Origin:           "http://127.0.0.1:27123",
		BackendAuthority: "0.0.0.0:7374",
		AllowedHosts:     []string{"spoke-a:7374"},
	})
	require.NoError(t, err)

	for host, want := range map[string]int{
		"spoke-a:7374":          http.StatusNoContent,
		"attacker.example:7374": http.StatusBadRequest,
	} {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:27123/api/v1/instance", nil)
		request.Host = host
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		assert.Equal(t, want, response.Code, host)
	}
}

func TestUnixSocketOriginPolicy(t *testing.T) {
	handler, err := ApplyListenerPolicy(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ListenerPolicy{Kind: ListenerSocket})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "http://kata.invalid/api/v1/ping", nil)
	request.Header.Set("Origin", "http://127.0.0.1:27123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	assert.Equal(t, http.StatusForbidden, response.Code)
}

func TestCheckWebStartup(t *testing.T) {
	require.NoError(t, CheckWebStartup("127.0.0.1:27123", config.AuthConfig{}, false))
	require.NoError(t, CheckWebStartup("100.64.0.5:27123", config.AuthConfig{
		Token: "configured-token", TrustPrivateNetwork: true,
	}, false))

	for _, address := range []string{"100.64.0.5:27123", "0.0.0.0:27123", "8.8.8.8:27123", "daemon.example:27123"} {
		t.Run(address, func(t *testing.T) {
			err := CheckWebStartup(address, config.AuthConfig{}, false)
			require.Error(t, err)
		})
	}
}

func TestServerListenerFailureStopsSibling(t *testing.T) {
	first, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	second, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &Server{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- server.ServeListeners(ctx,
			ListenerBinding{Listener: first, Policy: ListenerPolicy{Kind: ListenerSocket}},
			ListenerBinding{Listener: second, Policy: ListenerPolicy{Kind: ListenerSocket}},
		)
	}()

	require.NoError(t, first.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("listener failure did not stop the server")
	}
	_, err = net.DialTimeout("tcp", second.Addr().String(), 100*time.Millisecond)
	require.Error(t, err)
}
