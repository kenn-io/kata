package daemon_test

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

// A selected file failure must explain the repair without exposing daemon-host
// paths, falling back to another credential, or contacting the hub.
func TestFederationBridgeCatalogCredentialFailure(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"missing", "cannot read credential file"},
		{"absent", "selected hub has no user credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testenv.New(t)
			t.Setenv("EXAMPLE_BRIDGE_FALLBACK", "fallback-test-token")
			path := filepath.Join(t.TempDir(), "private-host-token")
			var calls atomic.Int32
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "unexpected hub request", http.StatusServiceUnavailable)
			}))
			t.Cleanup(hub.Close)
			catalog := config.CatalogDaemonConfig{Name: "selected-hub", URL: hub.URL, TokenFile: path, TokenEnv: "EXAMPLE_BRIDGE_FALLBACK", AllowInsecure: true}
			if tc.name == "absent" {
				catalog.TokenFile, catalog.TokenEnv = "", ""
			}
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCatalog: []config.CatalogDaemonConfig{catalog}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			local := httptest.NewServer(server.Handler())
			t.Cleanup(local.Close)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, local.URL+"/api/v1/federation/bridges", strings.NewReader(`{"hub_catalog":"selected-hub","hub_project":"hub-project","project_name":"spoke-project","actor":"local-member","preflight":true}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer local-owner-test-token")
			request.Header.Set("Content-Type", "application/json")
			response, err := local.Client().Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			assert.Equal(t, http.StatusBadRequest, response.StatusCode, string(body))
			var envelope api.ErrorEnvelope
			require.NoError(t, json.Unmarshal(body, &envelope))
			assert.Equal(t, "hub_credential_required", envelope.Error.Code)
			assert.Contains(t, envelope.Error.Message, tc.reason)
			assert.NotContains(t, string(body), path)
			assert.NotContains(t, string(body), "EXAMPLE_BRIDGE_FALLBACK")
			assert.Zero(t, calls.Load(), "credential rejection precedes every hub request")
		})
	}
}
