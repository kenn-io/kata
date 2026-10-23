package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
)

// Core actions count committed mutations once, and callers without a surface count nothing.
func TestCoreActionTelemetry(t *testing.T) {
	for _, surface := range []string{"web", ""} {
		t.Run(surface, func(t *testing.T) {
			store := openAuthTestDB(t)
			project, err := store.CreateProject(t.Context(), "example-project")
			require.NoError(t, err)
			reporter := &fakeTelemetryReporter{}
			if surface == "" {
				reporter.failNext = true
			}
			server := newTelemetryTestServer(t, reporter, Principal{Kind: PrincipalWebLocal}, store)
			path := fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
			send := func(route, body, key string, status int) api.MutationResponse {
				t.Helper()
				response := coreActionPost(t, server, route, body, surface, key)
				require.Equal(t, status, response.Code, response.Body.String())
				var out api.MutationResponse
				if status == http.StatusOK {
					require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out.Body))
				}
				return out
			}
			created := send(path, `{"title":"Example task","actor":"user-a"}`, "create-key", 200)
			require.True(t, created.Body.Changed)
			if surface == "" {
				require.True(t, reporter.failNext, "no header must skip capture entirely")
			}
			count := 0
			if surface == "web" {
				count = 1
			}
			require.Len(t, reporter.captured, count)
			if count > 0 {
				assert.Equal(t, capturedTelemetryEvent{event: "issue_created", properties: map[string]any{"surface": surface}}, reporter.captured[0])
			}
			send(path, `{"title":"Example task","actor":"user-a"}`, "create-key", 200)
			assert.Len(t, reporter.captured, count, "replay counts nothing")
			closePath := path + "/" + created.Body.Issue.ShortID + "/actions/close"
			body := `{"actor":"user-a","reason":"done","retry_protocol":"close-v1","message":"Completed the requested work and checked the result.","evidence":[{"type":"commit","sha":"abc1234"}]}`
			closed := send(closePath, body, "close-key", 200)
			require.True(t, closed.Body.Changed)
			if surface == "" {
				require.True(t, reporter.failNext, "no header must skip capture entirely")
			}
			assert.Len(t, reporter.captured, 2*count)
			send(closePath, body, "close-key", 200)
			noop := send(closePath, body, "", 200)
			assert.False(t, noop.Body.Changed)
			assert.Len(t, reporter.captured, 2*count, "replay and no-op count nothing")
			send(path+"/"+created.Body.Issue.ShortID+"/actions/reopen", `{"actor":"user-a"}`, "", 200)
			assert.Len(t, reporter.captured, 2*count)
			if count > 0 {
				assert.Equal(t, capturedTelemetryEvent{event: "issue_closed", properties: map[string]any{"surface": surface}}, reporter.captured[1])
			}
		})
	}
}

func coreActionPost(t *testing.T, s telemetryTestServer, path, body, surface, key string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, telemetryTestOrigin+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:40123"
	request.Header.Set("Origin", telemetryTestOrigin)
	request.Header.Set("Content-Type", "application/json")
	if surface != "" {
		request.Header.Set("X-Kata-Surface", surface)
	}
	request.Header.Set("Idempotency-Key", key)
	request.AddCookie(s.manager.Cookie(s.session.Cookie))
	request.Header.Set(webSessionHeader, s.session.Session)
	request.Header.Set(webCSRFHeader, s.session.CSRF)
	response := httptest.NewRecorder()
	s.handler.ServeHTTP(response, request)
	return response
}

func TestCoreActionTelemetryCaptureFailure(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	reporter := &fakeTelemetryReporter{failNext: true}
	server := newTelemetryTestServer(t, reporter, Principal{Kind: PrincipalWebLocal}, store)
	response := coreActionPost(t, server, fmt.Sprintf("/api/v1/projects/%d/issues", project.ID), `{"title":"Example task","actor":"user-a"}`, "cli", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.False(t, reporter.failNext, "capture was attempted")
	var out api.MutationResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out.Body))
	reporter.failNext = true
	response = coreActionPost(t, server, fmt.Sprintf("/api/v1/projects/%d/issues/%s/actions/close", project.ID, out.Body.Issue.ShortID), `{"actor":"user-a","reason":"done","retry_protocol":"close-v1","message":"Completed the requested work and checked the result.","evidence":[{"type":"commit","sha":"abc1234"}]}`, "tui", "")
	require.Equal(t, 200, response.Code, response.Body.String())
	assert.False(t, reporter.failNext)
}

func TestCoreActionTelemetryScopedMutations(t *testing.T) {
	store := openAuthTestDB(t)
	project, err := store.CreateProject(t.Context(), "example-project")
	require.NoError(t, err)
	root := createScopedAuthIssue(t, store, project.ID, "Example task", nil)
	_ = withScopedAuthorizationTestPrincipal(t, store, project, root)
	reporter := &fakeTelemetryReporter{}
	server := NewServer(ServerConfig{DB: store, Telemetry: reporter, Auth: config.AuthConfig{Token: "bootstrap-token", RequireTokenIdentity: true}})
	t.Cleanup(func() { _ = server.Close() })
	post := func(path, body string) api.MutationResponse {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.RemoteAddr = "127.0.0.1:40123"
		request.Header.Set("Authorization", "Bearer scoped-authorization-test-token")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Kata-Surface", "cli")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		require.Equal(t, 200, response.Code, response.Body.String())
		var out api.MutationResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &out.Body))
		require.True(t, out.Body.Changed)
		assert.Empty(t, reporter.captured)
		return out
	}
	path := fmt.Sprintf("/api/v1/projects/%d/issues", project.ID)
	created := post(path, fmt.Sprintf(`{"title":"Child task","links":[{"type":"parent","to_ref":%q}]}`, root.ShortID))
	post(path+"/"+created.Body.Issue.ShortID+"/actions/close", `{"reason":"done","message":"Completed the requested work and checked the result.","evidence":[{"type":"commit","sha":"abc1234"}]}`)
}

func TestCoreActionTelemetryRejectsClientPosts(t *testing.T) {
	reporter := &fakeTelemetryReporter{}
	server := newTelemetryTestServer(t, reporter, Principal{Kind: PrincipalWebLocal})
	for _, event := range []string{"issue_created", "issue_closed", "setup_finished", "sync_failed"} {
		response := server.post(t.Context(), t, fmt.Sprintf(`{"event":%q,"properties":{"surface":"web"}}`, event))
		require.Equal(t, 400, response.Code)
		assert.Equal(t, "unsupported_telemetry_event", decodeErrorCode(t, response))
	}
	require.Empty(t, reporter.captured)
}
