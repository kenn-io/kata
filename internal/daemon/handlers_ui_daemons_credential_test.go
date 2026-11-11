package daemon_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

func TestWebDaemonProxyMissingCatalogFileStopsBeforeRequest(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"contract_version":%q,"cursor":7,"capabilities":{"writable":false,"updates":"poll","actor_policy":"request"}}`, api.UISnapshotContractVersion)
	}))
	t.Cleanup(upstream.Close)
	missing := filepath.Join(t.TempDir(), "missing-token")
	d := openTestDB(t)
	server := startTestServer(t, daemon.ServerConfig{DB: d.db, StartedAt: d.now, WebDaemons: []config.CatalogDaemonConfig{{
		Name: "example-remote", URL: upstream.URL, TokenFile: missing, AllowInsecure: true,
	}}})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/ui/proxy/api/v1/ui/snapshot?view=all-open", nil)
	require.NoError(t, err)
	req.Header.Set("X-Kata-Web-Daemon", "example-remote")
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Zero(t, calls.Load(), "failed selected credential must stop before an upstream request")
	assert.Equal(t, http.StatusBadRequest, response.StatusCode, string(body))
	assert.Contains(t, string(body), "daemon_auth_required")
	assert.NotContains(t, string(body), missing)
}
