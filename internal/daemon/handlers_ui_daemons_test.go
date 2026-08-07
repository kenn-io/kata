package daemon_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

func TestWebDaemonRosterIsSanitizedAndReportsHealth(t *testing.T) {
	connected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/instance", r.URL.Path)
		assert.Equal(t, "Bearer target-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"instance_uid":"01J00000000000000000000001","web_ui_contract_version":"1"}`)
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

func TestWebDaemonRosterReportsTargetsWithoutWebUIContract(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"instance_uid":"01J00000000000000000000001"}`)
	}))
	t.Cleanup(legacy.Close)

	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{
			{Name: "example-local", Local: true},
			{Name: "example-remote", URL: legacy.URL, AllowInsecure: true},
		},
	})

	response, err := http.Get(server.URL + "/api/v1/ui/daemons")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	var roster struct {
		Daemons []struct {
			Health string `json:"health"`
			Hint   string `json:"hint"`
		} `json:"daemons"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&roster))
	require.Len(t, roster.Daemons, 2)
	assert.Equal(t, "upgrade_required", roster.Daemons[1].Health)
	assert.Equal(t, "daemon does not support the Kata web UI", roster.Daemons[1].Hint)
}

func TestAnonymousReadonlyRosterHidesConfiguredCredentialTargetsBeforeProbing(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"capabilities":{"writable":true,"updates":"sse","actor_policy":"request"}}`)
	}))
	t.Cleanup(upstream.Close)

	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now, InsecureReadonly: true, ActiveWebDaemon: "example-remote",
		WebDaemons: []config.CatalogDaemonConfig{
			{Name: "example-local", Local: true},
			{Name: "example-remote", URL: upstream.URL, TokenEnv: "EXAMPLE_UNSET_TOKEN", AllowInsecure: true},
		},
	})

	response, err := http.Get(server.URL + "/api/v1/ui/daemons")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	var roster struct {
		Daemons []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"daemons"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&roster))
	require.Len(t, roster.Daemons, 1)
	assert.Equal(t, "example-local", roster.Daemons[0].ID)
	assert.True(t, roster.Daemons[0].Default)
	assert.Equal(t, int64(0), calls.Load())

	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/ui/snapshot?view=all-open", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	response, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Equal(t, int64(0), calls.Load())
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
		assert.Empty(t, r.Header.Get("X-Kata-Actor"))
		assert.Empty(t, r.Header.Get("X-Example-Proxy"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Set-Cookie", "upstream=secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"contract_version":"1","cursor":7,"capabilities":{"writable":true,"updates":"sse","actor_policy":"request"}}`)
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
	request.Header.Set("X-Kata-Actor", "user-a")
	request.Header.Set("X-Example-Proxy", "user-a")
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	assert.JSONEq(t, `{"contract_version":"1","cursor":7,"capabilities":{"writable":true,"updates":"poll","actor_policy":"request"}}`, string(body))
	assert.Empty(t, response.Header.Get("Set-Cookie"))
}

func TestWebDaemonProxyRestrictsDownstreamOperations(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"web_ui_contract_version": api.UISnapshotContractVersion,
				"web_ui_capabilities": api.UICapabilities{
					Writable: true, Updates: "poll", ActorPolicy: "request",
				},
			})
			return
		}
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
		{name: "project lookup", path: "/api/v1/projects/resolve", body: `{"name":"example-project"}`, want: http.StatusNoContent},
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
	assert.Equal(t, int64(2), calls.Load())
}

func TestWebDaemonGatewayAllowsIssueReferenceLookup(t *testing.T) {
	d := openTestDB(t)
	project, err := d.db.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	issue, _, err := d.db.CreateIssue(t.Context(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "Example issue", Author: "user-a",
	})
	require.NoError(t, err)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{Name: "example-local", Local: true}},
	})

	request, err := http.NewRequest(http.MethodGet, fmt.Sprintf(
		"%s/api/v1/ui/proxy/api/v1/ui/issue-reference?project_id=%d&ref=%s", server.URL, project.ID, issue.ShortID,
	), nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-local")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	assert.Equal(t, map[string]any{
		"issue": map[string]any{"uid": issue.UID, "project_uid": project.UID},
	}, body)
}

func TestWebDaemonGatewayRejectsDownstreamRosterBeforeProxying(t *testing.T) {
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
			Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
		}},
	})

	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/ui/daemons", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Equal(t, int64(0), calls.Load())
}

func TestWebDaemonGatewayRejectsDeletedIssueLookupBeforeProxying(t *testing.T) {
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
			Name: "example-remote", URL: upstream.URL, Token: "target-token", AllowInsecure: true,
		}},
	})

	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/ui/proxy/api/v1/projects/7/issues/abc4?include_deleted=true", nil)
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Equal(t, int64(0), calls.Load())
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

func TestWebDaemonProxyRejectsUpstreamRedirects(t *testing.T) {
	var redirectedCalls atomic.Int64
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(redirected.Close)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirected.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)

	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{
		DB: d.db, StartedAt: d.now,
		WebDaemons: []config.CatalogDaemonConfig{{
			Name: "example-remote", URL: upstream.URL, AllowInsecure: true,
		}},
	})
	request, err := http.NewRequest(http.MethodPost,
		server.URL+"/api/v1/ui/proxy/api/v1/projects/resolve",
		strings.NewReader(`{"name":"example-project"}`))
	require.NoError(t, err)
	request.Header.Set("X-Kata-Web-Daemon", "example-remote")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadGateway, response.StatusCode, string(body))
	assert.Contains(t, string(body), "daemon_redirect_forbidden")
	assert.Equal(t, int64(0), redirectedCalls.Load())
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
