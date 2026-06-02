package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestAuthMiddleware_NoTokenConfigured_AllRequestsPass(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "", InsecureReadonly: false})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_TokenConfigured_MissingHeader_401(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Contains(t, rr.Body.String(), `"auth_required"`)
}

func TestAuthMiddleware_TokenConfigured_WrongToken_403(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

func TestAuthMiddleware_TokenConfigured_CorrectToken_OK(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer expected-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_IdentityModeDBTokenSetsPrincipal(t *testing.T) {
	d := openAuthTestDB(t)
	_, _, err := d.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "user-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		require.True(t, ok)
		assert.Equal(t, PrincipalDBToken, principal.Kind)
		assert.Equal(t, "alice", principal.Actor)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer user-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_IdentityModeMissingBearer401(t *testing.T) {
	d := openAuthTestDB(t)
	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Contains(t, rr.Body.String(), `"auth_required"`)
}

func TestAuthMiddleware_IdentityModeUnknownToken403(t *testing.T) {
	d := openAuthTestDB(t)
	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer unknown-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Contains(t, rr.Body.String(), `"token_invalid"`)
}

func TestAuthMiddleware_IdentityModeTokenLookupError500(t *testing.T) {
	d := openAuthTestDB(t)
	_, err := d.ExecContext(context.Background(), `DROP TABLE api_tokens`)
	require.NoError(t, err)

	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer user-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
	assert.Contains(t, rr.Body.String(), `"internal"`)
}

func TestAuthMiddleware_IdentityModeRevokedToken403(t *testing.T) {
	d := openAuthTestDB(t)
	tok, _, err := d.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: "revoked-token",
		Actor:          "alice",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)
	_, _, err = d.RevokeAPIToken(context.Background(), tok.ID, db.BootstrapActor)
	require.NoError(t, err)

	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer revoked-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.Contains(t, rr.Body.String(), `"token_invalid"`)
}

func TestAuthMiddleware_IdentityModeBootstrapTokenSetsPrincipal(t *testing.T) {
	d := openAuthTestDB(t)
	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		require.True(t, ok)
		assert.Equal(t, PrincipalBootstrap, principal.Kind)
		assert.Empty(t, principal.Actor)
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer bootstrap-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_IdentityModeBootstrapTokenReachesPostHandlers(t *testing.T) {
	d := openAuthTestDB(t)
	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/resolve", nil)
	req.Header.Set("Authorization", "Bearer bootstrap-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_IdentityModeBootstrapTokenCanAdminTokens(t *testing.T) {
	d := openAuthTestDB(t)
	mw := requireBearer(authPolicy{Token: "bootstrap-token", RequireTokenIdentity: true}, d)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", nil)
	req.Header.Set("Authorization", "Bearer bootstrap-token")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestAuthMiddleware_InsecureReadonly_GETPasses_POSTAndSSERejected(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "", InsecureReadonly: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/projects", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/stream", nil)
	req.Header.Set("Accept", "text/event-stream")
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestAuthMiddleware_InsecureReadonly_TokenAdminGETRejected(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "", InsecureReadonly: true})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/tokens", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Contains(t, rr.Body.String(), `"auth_required"`)
}

func TestAuthMiddleware_UnauthenticatedPathsAlwaysPass(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/api/v1/ping", "/api/v1/health"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		assert.Equal(t, http.StatusOK, rr.Code, "unauthenticated path %s should pass", p)
	}
}

func TestAuthMiddleware_FederationTransportPathsBypassAdminBearer(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/projects/1/federation/events"},
		{method: http.MethodGet, path: "/api/v1/projects/1/federation/metadata"},
		{method: http.MethodPost, path: "/api/v1/projects/1/federation/events:ingest"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		assert.Equal(t, http.StatusAccepted, rr.Code, "%s %s should bypass daemon bearer middleware", tc.method, tc.path)
	}
}

func TestAuthMiddleware_FederationTransportBypassRequiresExactRouteAndMethod(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "wrong method for poll", method: http.MethodPost, path: "/api/v1/projects/1/federation/events"},
		{name: "wrong method for metadata", method: http.MethodPost, path: "/api/v1/projects/1/federation/metadata"},
		{name: "wrong method for ingest", method: http.MethodGet, path: "/api/v1/projects/1/federation/events:ingest"},
		{name: "extra suffix segment", method: http.MethodGet, path: "/api/v1/projects/1/federation/events/extra"},
		{name: "extra middle segment", method: http.MethodGet, path: "/api/v1/projects/1/setup/federation/events"},
		{name: "nonnumeric project id", method: http.MethodGet, path: "/api/v1/projects/not-a-number/federation/events"},
		{name: "admin enrollment setup", method: http.MethodPost, path: "/api/v1/federation/enrollments"},
		{name: "admin replica setup", method: http.MethodPost, path: "/api/v1/federation/replicas"},
		{name: "project federation enable", method: http.MethodPost, path: "/api/v1/projects/1/federation/enable"},
		{name: "project federation metadata", method: http.MethodGet, path: "/api/v1/projects/1/federation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, http.StatusUnauthorized, rr.Code, "%s %s should require daemon bearer", tc.method, tc.path)
		})
	}
}

func TestAuthMiddleware_FederationSetupRouteUsesAdminBearer(t *testing.T) {
	mw := requireBearer(authPolicy{Token: "expected-token"})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/federation/enrollments", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/federation/enrollments", nil)
	req.Header.Set("Authorization", "Bearer expected-token")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusAccepted, rr.Code)
}

func openAuthTestDB(t *testing.T) *sqlitestore.Store {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	d, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}
