package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

// TestSelfAuthenticatedRouteMatcherMatchesOnlyTransportRoutes uses literal
// requests and expectations independent of the rule and route tables that
// generate the matcher. Removing or misrouting any self-authenticating
// operation, or broadening a route to the wrong method, makes this test fail.
func TestSelfAuthenticatedRouteMatcherMatchesOnlyTransportRoutes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	matcher := srv.authPolicy.SelfAuthenticatedRoutes

	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"federation metadata", http.MethodGet,
			"/api/v1/projects/7/federation/metadata", true},
		{"federation poll", http.MethodGet,
			"/api/v1/projects/7/federation/events", true},
		{"federation ingest", http.MethodPost,
			"/api/v1/projects/7/federation/events:ingest", true},
		{"lease acquire", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/acquire", true},
		{"lease renew", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/renew", true},
		{"lease release", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/release", true},
		{"lease status", http.MethodGet,
			"/api/v1/projects/7/issues/abc4/lease", true},
		{"lease force release", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/force_release", true},
		{"issue read requires daemon bearer", http.MethodGet,
			"/api/v1/projects/7/issues/abc4", false},
		{"federation setup requires daemon bearer", http.MethodPost,
			"/api/v1/federation/enrollments", false},
		{"project federation config requires daemon bearer", http.MethodGet,
			"/api/v1/projects/7/federation", false},
		{"poll rejects post", http.MethodPost,
			"/api/v1/projects/7/federation/events", false},
		{"ingest rejects get", http.MethodGet,
			"/api/v1/projects/7/federation/events:ingest", false},
		{"lease acquire rejects get", http.MethodGet,
			"/api/v1/projects/7/issues/abc4/lease/actions/acquire", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, nil)
			assert.Equal(t, tc.want, matcher.matches(request))
		})
	}
}

func TestSelfAuthenticatedRouteMatcherZeroValueMatchesNothing(t *testing.T) {
	var matcher selfAuthenticatedRouteMatcher
	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/1/federation/events", nil)
	assert.False(t, matcher.matches(request))
}

// TestBearerMiddlewareRequiresExactMethodForSelfAuthenticatedGETRoutes catches
// net/http's GET-pattern behavior broadening the bypass to HEAD. The deleted
// parsers required exact GET, so HEAD must continue to require the daemon token.
func TestBearerMiddlewareRequiresExactMethodForSelfAuthenticatedGETRoutes(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	policy := authPolicy{
		Token: "daemon-token",
		SelfAuthenticatedRoutes: newSelfAuthenticatedRouteMatcher(
			selfAuthenticatedRoutes(srv.API().OpenAPI())),
	}
	handler := requireBearer(policy)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))

	for _, tc := range []struct {
		name string
		path string
	}{
		{"lease status", "/api/v1/projects/7/issues/abc4/lease"},
		{"federation poll", "/api/v1/projects/7/federation/events"},
		{"federation metadata", "/api/v1/projects/7/federation/metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, tc.path, nil))
			assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}

// TestBearerMiddlewareRequiresTokenForUnusableProjectID pins the guard the
// deleted digit scans provided: a handler can only authenticate a request
// against a project it can identify, so an unusable {project_id} keeps
// requiring the daemon's bearer token.
func TestBearerMiddlewareRequiresTokenForUnusableProjectID(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	policy := authPolicy{
		Token: "daemon-token",
		SelfAuthenticatedRoutes: newSelfAuthenticatedRouteMatcher(
			selfAuthenticatedRoutes(srv.API().OpenAPI())),
	}
	handler := requireBearer(policy)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))

	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"lease status", http.MethodGet,
			"/api/v1/projects/7/issues/abc4/lease", http.StatusAccepted},
		{"lease status non-numeric project", http.MethodGet,
			"/api/v1/projects/spoke/issues/abc4/lease", http.StatusUnauthorized},
		{"lease acquire", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/acquire", http.StatusAccepted},
		{"lease acquire non-numeric project", http.MethodPost,
			"/api/v1/projects/spoke/issues/abc4/lease/actions/acquire", http.StatusUnauthorized},
		{"lease force release", http.MethodPost,
			"/api/v1/projects/7/issues/abc4/lease/actions/force_release", http.StatusAccepted},
		{"federation poll", http.MethodGet,
			"/api/v1/projects/7/federation/events", http.StatusAccepted},
		{"federation poll non-numeric project", http.MethodGet,
			"/api/v1/projects/spoke/federation/events", http.StatusUnauthorized},
		{"federation metadata non-numeric project", http.MethodGet,
			"/api/v1/projects/spoke/federation/metadata", http.StatusUnauthorized},
		{"federation ingest non-numeric project", http.MethodPost,
			"/api/v1/projects/spoke/federation/events:ingest", http.StatusUnauthorized},
		{"federation metadata zero project", http.MethodGet,
			"/api/v1/projects/0/federation/metadata", http.StatusUnauthorized},
		{"wrong method for poll", http.MethodPost,
			"/api/v1/projects/7/federation/events", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, tc.want, recorder.Code)
		})
	}
}

// TestHostAccessRejectsFederationBearerOnForceReleaseOnly is the HTTP-level
// half of the force-release asymmetry: the bearer middleware defers to the
// handler for both routes, but the host-access middleware only dispatches an
// unattributed bearer for routes that accept an enrollment credential.
func TestHostAccessRejectsFederationBearerOnForceReleaseOnly(t *testing.T) {
	store := openAuthTestDB(t)
	srv := NewServer(ServerConfig{DB: store, HostAccess: allowAllHostAccess{}})
	t.Cleanup(func() { _ = srv.Close() })

	for _, tc := range []struct {
		name       string
		path       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "acquire reaches the handler's enrollment check",
			path:       "/api/v1/projects/1/issues/abc4/lease/actions/acquire",
			wantStatus: http.StatusForbidden,
			wantCode:   "auth_invalid",
		},
		{
			name:       "force release is refused before dispatch",
			path:       "/api/v1/projects/1/issues/abc4/lease/actions/force_release",
			wantStatus: http.StatusUnauthorized,
			wantCode:   "authentication_required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, tc.path,
				strings.NewReader(`{"holder":"spoke-holder"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer enrollment-token")
			recorder := httptest.NewRecorder()
			srv.Handler().ServeHTTP(recorder, request)
			assert.Equal(t, tc.wantStatus, recorder.Code, "body: %s", recorder.Body.String())
			assert.Contains(t, recorder.Body.String(), tc.wantCode)
		})
	}
}

// TestFederationIngestPreauthParserMatchesRegisteredRoute pins the one route
// parser that deliberately does not consume the matcher (see the comment on
// federationIngestProjectID) against the template the route is registered with.
func TestFederationIngestPreauthParserMatchesRegisteredRoute(t *testing.T) {
	srv := NewServer(ServerConfig{})
	t.Cleanup(func() { _ = srv.Close() })
	template, ok := registeredOperations(srv.API().OpenAPI())["ingestFederationProjectEvents"]
	require.True(t, ok, "ingestFederationProjectEvents is not registered")

	projectID, matched, valid := federationIngestProjectID(
		template.Method, strings.ReplaceAll(template.Path, "{project_id}", "42"))

	assert.Truef(t, matched, "preauth parser no longer matches the registered route %s %s",
		template.Method, template.Path)
	assert.True(t, valid)
	assert.Equal(t, int64(42), projectID)
}

type allowAllHostAccess struct{}

func (allowAllHostAccess) Authorize(
	context.Context,
	HostAccessRequest,
) (HostAccessDecision, error) {
	return HostAccessDecision{
		Revalidate:       func(context.Context) error { return nil },
		TransactionFence: func(context.Context, db.Transaction) error { return nil },
	}, nil
}
