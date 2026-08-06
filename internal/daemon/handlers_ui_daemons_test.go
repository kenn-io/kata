package daemon_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

func TestWebDaemonRosterIsSanitizedAndReportsHealth(t *testing.T) {
	connected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/instance", r.URL.Path)
		assert.Equal(t, "Bearer target-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"instance_uid":"01J00000000000000000000001"}`)
	}))
	t.Cleanup(connected.Close)
	authRequired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(authRequired.Close)

	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, ActiveWebDaemon: "example-remote",
		WebDaemons: []config.CatalogDaemonConfig{
			{Name: "example-local", Local: true},
			{Name: "example-remote", URL: connected.URL, Token: "target-token", AllowInsecure: true},
			{Name: "example-auth", URL: authRequired.URL, TokenEnv: "EXAMPLE_DAEMON_TOKEN", AllowInsecure: true},
		},
	})

	response, err := http.Get(server.URL + "/api/v1/ui/daemons")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	assert.NotContains(t, string(body), "target-token")
	assert.NotContains(t, string(body), "EXAMPLE_DAEMON_TOKEN")

	var roster struct {
		Daemons []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
			Auth    string `json:"auth"`
			Health  string `json:"health"`
		} `json:"daemons"`
	}
	require.NoError(t, json.Unmarshal(body, &roster))
	require.Len(t, roster.Daemons, 3)
	assert.Equal(t, "connected", roster.Daemons[0].Health)
	assert.Equal(t, "connected", roster.Daemons[1].Health)
	assert.True(t, roster.Daemons[1].Default)
	assert.Equal(t, "token", roster.Daemons[1].Auth)
	assert.Equal(t, "auth_required", roster.Daemons[2].Health)
}

func TestWebDaemonProxyPinsTargetAndStripsBrowserCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/ui/snapshot", r.URL.Path)
		assert.Equal(t, "view=all-open", r.URL.RawQuery)
		assert.Equal(t, "Bearer target-token", r.Header.Get("Authorization"))
		assert.Empty(t, r.Header.Get("Origin"))
		assert.Empty(t, r.Header.Get("Cookie"))
		assert.Empty(t, r.Header.Get("X-Kata-Web-Session"))
		assert.Empty(t, r.Header.Get("X-Kata-CSRF"))
		assert.Empty(t, r.Header.Get("X-Kata-Web-Daemon"))
		w.Header().Set("Set-Cookie", "upstream=secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"cursor":7}`)
	}))
	t.Cleanup(upstream.Close)

	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
		}},
	})
	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/ui/snapshot?view=all-open", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	request.Header.Set("Authorization", "Bearer browser-token")
	request.Header.Set("Cookie", "kata=browser-cookie")
	request.Header.Set("X-Kata-Web-Session", "browser-session")
	request.Header.Set("X-Kata-CSRF", "browser-csrf")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	assert.JSONEq(t, `{"cursor":7}`, string(body))
	assert.Empty(t, response.Header.Get("Set-Cookie"))
}

func TestWebDaemonProxyRestrictsDownstreamOperations(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)
	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, AllowInsecure: true,
		}},
	})

	for _, test := range []struct {
		name string
		path string
		body string
		want int
	}{
		{name: "ordinary metadata", path: "/api/v1/projects/7/metadata", body: `{}`, want: http.StatusNoContent},
		{name: "filesystem project", path: "/api/v1/projects", body: `{"start_path":" "}`, want: http.StatusForbidden},
		{name: "federation", path: "/api/v1/federation/replicas", body: `{}`, want: http.StatusForbidden},
		{name: "purge", path: "/api/v1/projects/7/actions/purge", body: `{}`, want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost,
				server.URL+"/api/v1/ui/proxy"+test.path, strings.NewReader(test.body))
			require.NoError(t, err)
			request.Header.Set("X-Kata-Web-Daemon", "example-remote")
			request.Header.Set("Content-Type", "application/json")
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			_ = response.Body.Close()
			assert.Equal(t, test.want, response.StatusCode)
		})
	}
	assert.Equal(t, int64(1), calls.Load())
}

func TestWebDaemonProxyDoesNotMisclassifyUpstreamAuthAsBrowserExpiry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)
	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, Token: "invalid-token", AllowInsecure: true,
		}},
	})
	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/ui/snapshot", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadGateway, response.StatusCode)
	assert.Contains(t, string(body), "daemon_auth_required")
}

func TestWebDaemonProxyDispatchesLocalSelectionInProcess(t *testing.T) {
	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{Name: "example-local", Local: true}},
	})
	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/ping", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-local")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusOK, response.StatusCode)
}
