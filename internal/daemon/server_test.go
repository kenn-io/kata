package daemon_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_PingReturnsOK(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	resp, body := getStatusBody(t, ts, "/api/v1/ping")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"ok":true`)
}

func TestServer_RejectsNonEmptyOrigin(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	resp, _ := doReq(t, ts, http.MethodGet, "/api/v1/ping", nil, map[string]string{
		"Origin": "https://attacker.example.com",
	})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestServer_MutationRequiresJSON(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	resp, err := http.Post(ts.URL+"/api/v1/projects/resolve", "text/plain",
		strings.NewReader(`{"start_path":"/x"}`))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
}

func TestAttributedWriteDoesNotApplyFederationTokenActorPolicy(t *testing.T) {
	resp := createIssueWithActor(t, "bootstrap")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func createIssueWithActor(t *testing.T, actor string) *http.Response {
	t.Helper()
	ts, handle := startDefaultTestServer(t)
	project, err := handle.db.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	resp, _ := doReq(t, ts, http.MethodPost, issuesURL(project.ID), map[string]any{
		"actor": actor,
		"title": "example issue",
	}, nil)
	return resp
}
