package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestServer_ExposesStandardPprofHandlers(t *testing.T) {
	ts, _ := startDefaultTestServer(t)

	for _, tc := range []struct {
		path               string
		wantContentType    string
		wantDisposition    string
		wantBodySubstring  string
		cancelBeforeHandle bool
	}{
		{
			path:              "/debug/pprof/",
			wantContentType:   "text/html; charset=utf-8",
			wantBodySubstring: "Types of profiles available:",
		},
		{
			path:            "/debug/pprof/heap",
			wantContentType: "application/octet-stream",
			wantDisposition: `attachment; filename="heap"`,
		},
		{
			path:              "/debug/pprof/cmdline",
			wantContentType:   "text/plain; charset=utf-8",
			wantBodySubstring: "daemon.test",
		},
		{
			path:              "/debug/pprof/symbol",
			wantContentType:   "text/plain; charset=utf-8",
			wantBodySubstring: "num_symbols:",
		},
		{
			path:               "/debug/pprof/profile?seconds=1",
			wantContentType:    "application/octet-stream",
			wantDisposition:    `attachment; filename="profile"`,
			cancelBeforeHandle: true,
		},
		{
			path:               "/debug/pprof/trace?seconds=1",
			wantContentType:    "application/octet-stream",
			wantDisposition:    `attachment; filename="trace"`,
			cancelBeforeHandle: true,
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.cancelBeforeHandle {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			recorder := httptest.NewRecorder()

			ts.Config.Handler.ServeHTTP(recorder, req)

			resp := recorder.Result()
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, tc.wantContentType, resp.Header.Get("Content-Type"))
			assert.Equal(t, tc.wantDisposition, resp.Header.Get("Content-Disposition"))
			if tc.wantBodySubstring != "" {
				assert.Contains(t, recorder.Body.String(), tc.wantBodySubstring)
			}
		})
	}
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
