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

func openAuthTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}
