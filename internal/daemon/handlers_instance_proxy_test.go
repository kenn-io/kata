package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
)

func instanceWriteActorsVerifiedViaProxy(t *testing.T, auth config.AuthConfig, bearer string) (bool, string) {
	t.Helper()
	store := openAuthTestDB(t)
	server := NewServer(ServerConfig{DB: store, StartedAt: time.Now().UTC(), Auth: auth})
	defer func() { _ = server.Close() }()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/instance", nil)
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 27123}))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("X-Kata-Actor", "user-a")
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var out struct {
		WriteActorsVerified bool `json:"write_actors_verified"`
		Auth                struct {
			Kind string `json:"kind"`
		} `json:"auth"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out))
	return out.WriteActorsVerified, out.Auth.Kind
}

func TestInstanceWriteActorsVerifiedWithTrustedProxyActor(t *testing.T) {
	auth := config.AuthConfig{
		Token: "bootstrap-token",
		Proxy: config.ProxyConfig{
			TrustedActorHeader:    "X-Kata-Actor",
			TrustedProxyListeners: []string{"127.0.0.1:27123"},
		},
	}

	verified, kind := instanceWriteActorsVerifiedViaProxy(t, auth, "bootstrap-token")
	assert.Equal(t, "trusted_proxy", kind)
	assert.False(t, verified, "outside identity mode only the proxy listener takes its actor from a credential")

	auth.RequireTokenIdentity = true
	verified, kind = instanceWriteActorsVerifiedViaProxy(t, auth, "bootstrap-token")
	assert.Equal(t, "trusted_proxy", kind)
	assert.True(t, verified)
}
