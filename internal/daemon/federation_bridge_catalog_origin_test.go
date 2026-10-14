package daemon_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/testenv"
)

// A catalog URL selects a hub by its origin. Spelling the same origin with an
// uppercase host or an explicit default port still selects it, while a path,
// query or fragment is refused before the hub is contacted.
func TestFederationBridgeCatalogAcceptsEquivalentOriginSpellings(t *testing.T) {
	var contacted atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		http.Error(w, "stub hub", http.StatusServiceUnavailable)
	}))
	t.Cleanup(hub.Close)
	port := hub.URL[strings.LastIndex(hub.URL, ":")+1:]
	for name, tc := range map[string]struct {
		url      string
		accepted bool
	}{
		"uppercase host":        {url: "http://LOCALHOST:" + port, accepted: true},
		"trailing slash":        {url: "http://localhost:" + port + "/", accepted: true},
		"explicit default port": {url: "HTTPS://Hub.Example:443", accepted: true},
		"path prefix":           {url: "http://localhost:" + port + "/kata"},
		"query":                 {url: "http://localhost:" + port + "/?next=1"},
		"fragment":              {url: "http://localhost:" + port + "#hub"},
	} {
		t.Run(name, func(t *testing.T) {
			env := testenv.New(t)
			server := daemon.NewServer(daemon.ServerConfig{DB: env.DB, Auth: config.AuthConfig{Token: "local-owner-test-token"}, FederationCatalog: []config.CatalogDaemonConfig{
				{Name: "selected-hub", URL: tc.url, Token: "member-test-token", AllowInsecure: true},
			}})
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			local := httptest.NewServer(server.Handler())
			t.Cleanup(local.Close)
			before := contacted.Load()
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, local.URL+"/api/v1/federation/bridges",
				strings.NewReader(`{"hub_catalog":"selected-hub","hub_project":"hub-project","project_name":"spoke-project","actor":"local-member","preflight":true}`))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer local-owner-test-token")
			request.Header.Set("Content-Type", "application/json")
			response, err := local.Client().Do(request)
			require.NoError(t, err)
			body := readClose(t, response)
			refused := response.StatusCode == http.StatusBadRequest && strings.Contains(string(body), "hub_catalog must select a remote root HTTP(S) origin")
			require.Equal(t, !tc.accepted, refused, "status %d: %s", response.StatusCode, body)
			if !tc.accepted {
				require.Equal(t, before, contacted.Load(), "a refused catalog URL is never contacted")
			} else if !strings.Contains(tc.url, "Hub.Example") {
				require.Greater(t, contacted.Load(), before, "an accepted local origin is contacted")
			}
		})
	}
}
