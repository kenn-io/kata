package daemon_test

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// Clients label a connection without an account from the authentication
// kind, so each actorless mode must report a distinct, secret-free kind.
func TestActorlessConnectionsReportDistinctAuthKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config daemon.ServerConfig
		bearer string
		want   string
	}{
		{name: "owner_local", want: "none"},
		{name: "shared_token", config: daemon.ServerConfig{Auth: config.AuthConfig{Token: "shared-daemon-test-token"}}, bearer: "shared-daemon-test-token", want: "static_token"},
		{name: "insecure_readonly", config: daemon.ServerConfig{InsecureReadonly: true}, want: "insecure_readonly"},
		{name: "unauthenticated_private_network", config: daemon.ServerConfig{Auth: config.AuthConfig{AllowUnauthenticatedPrivateNetworkWrites: true}}, want: "unauthenticated_private_network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			tc.config.DB = store
			server := daemon.NewServer(tc.config)
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			for _, path := range []string{"/api/v1/instance", "/api/v1/ui/snapshot?view=all"} {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.RemoteAddr = "192.168.1.10:43210"
				request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("192.168.1.20"), Port: 7777}))
				if tc.bearer != "" {
					request.Header.Set("Authorization", "Bearer "+tc.bearer)
				}
				recorder := httptest.NewRecorder()
				server.Handler().ServeHTTP(recorder, request)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				var response struct {
					Auth struct {
						Kind string `json:"kind"`
					} `json:"auth"`
					Capabilities struct {
						AuthKind string `json:"auth_kind"`
					} `json:"capabilities"`
				}
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
				got := response.Auth.Kind
				if path != "/api/v1/instance" {
					got = response.Capabilities.AuthKind
				}
				require.Equal(t, tc.want, got, path)
				require.NotContains(t, recorder.Body.String(), "shared-daemon-test-token")
			}
		})
	}
}
