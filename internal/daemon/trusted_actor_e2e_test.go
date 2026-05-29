package daemon_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
)

// startTrustedProxyTestServer pre-binds a loopback TCP listener, then builds a
// daemon.Server whose trusted-proxy allowlist names that listener's address,
// and finally swaps the listener into an httptest.Server. The two-step dance
// exists because withTrustedProxyActor captures the allowlist at NewServer
// time; httptest.NewServer would otherwise pick its own random port and the
// allowlist entry would never match.
func startTrustedProxyTestServer(t *testing.T, headerName string) *httptest.Server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	d := openTestDB(t)
	cfg := daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		Auth: config.AuthConfig{
			Proxy: config.ProxyConfig{
				TrustedActorHeader:    headerName,
				TrustedProxyListeners: []string{l.Addr().String()},
			},
		},
	}
	srv := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, ts.Listener.Close())
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// bearerProxyOpts carries the per-test bearer policy for
// startBearerProxyTestServer. Token is the static or bootstrap value
// requireBearer compares the Authorization header against; identity-mode is
// opt-in (RequireTokenIdentity must be true AND Token must be non-empty since
// checkAuthStartup enforces that pairing, but Serve uses raw NewServer so the
// constraint is also relied on by the tests below).
type bearerProxyOpts struct {
	Token                string
	RequireTokenIdentity bool
}

// startBearerProxyTestServer is the sibling of startTrustedProxyTestServer
// that also wires bearer auth. The two layers stack in production: requireBearer
// runs first and admits/refuses; withTrustedProxyActor runs second and
// overwrites the principal on trusted listeners. None of the original
// trusted-proxy e2e tests exercise that stacking, so this helper exists to add
// targeted coverage. Returns the server and the DB so identity-mode tests can
// mint DB tokens against the same store the daemon resolves through.
func startBearerProxyTestServer(t *testing.T, headerName string, auth bearerProxyOpts) (*httptest.Server, *db.DB) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	d := openTestDB(t)
	cfg := daemon.ServerConfig{
		DB:        d.db,
		StartedAt: d.now,
		Auth: config.AuthConfig{
			Token:                auth.Token,
			RequireTokenIdentity: auth.RequireTokenIdentity,
			Proxy: config.ProxyConfig{
				TrustedActorHeader:    headerName,
				TrustedProxyListeners: []string{l.Addr().String()},
			},
		},
	}
	srv := daemon.NewServer(cfg)
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, ts.Listener.Close())
	ts.Listener = l
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, d.db
}

// trustedProxyCreateProject posts to /api/v1/projects with a path-free init
// (name only) so the test does not need a git workspace on disk. Returns the
// new project's rowid. The headers map is passed through to doReq so callers
// can carry an X-Kata-Actor header on the setup call when token-identity mode
// or trusted-proxy mode requires attribution.
func trustedProxyCreateProject(t *testing.T, ts *httptest.Server, headers map[string]string) int64 {
	t.Helper()
	resp, raw := doReq(t, ts, "POST", "/api/v1/projects",
		map[string]any{"name": "trusted-proxy-e2e"}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "init project: %s", raw)
	var body struct {
		Project struct {
			ID int64 `json:"id"`
		} `json:"project"`
	}
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	require.NotZero(t, body.Project.ID, "init returned zero project id: %s", raw)
	return body.Project.ID
}

// trustedProxyCreateIssue posts to /api/v1/projects/{id}/issues and returns
// the new issue's short_id, which is the wire-format ref the close action
// expects. Headers are forwarded to doReq so the call can carry the trusted
// actor header along with the body actor.
func trustedProxyCreateIssue(t *testing.T, ts *httptest.Server, projectID int64, headers map[string]string) string {
	t.Helper()
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues",
		map[string]any{"actor": "setup", "title": "trusted proxy header e2e"}, headers)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "create issue: %s", raw)
	var body struct {
		Issue struct {
			ShortID string `json:"short_id"`
		} `json:"issue"`
	}
	require.NoError(t, json.Unmarshal(raw, &body), string(raw))
	require.NotEmpty(t, body.Issue.ShortID, "create returned no short_id: %s", raw)
	return body.Issue.ShortID
}

// TestTrustedProxyHeader_CreditsHeaderActor exercises the full pipeline:
// withTrustedProxyActor turns the X-Kata-Actor header into a PrincipalTrustedProxy
// on the trusted listener, attributedActor prefers the principal's actor over
// the request body's actor, and the close event row records the header value
// rather than the body value.
func TestTrustedProxyHeader_CreditsHeaderActor(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	// Seed via the HTTP surface so the trusted-proxy principal is the
	// only writer in scope. initProject (POST /api/v1/projects) does
	// not call attributedActor, but the header is still applied by the
	// middleware so passing it here is harmless and keeps the test
	// uniform. createIssue does call attributedActor, so the header is
	// required for the seed issue to materialize.
	projectID := trustedProxyCreateProject(t, ts, map[string]string{"X-Kata-Actor": "setup"})
	issueRef := trustedProxyCreateIssue(t, ts, projectID,
		map[string]string{"X-Kata-Actor": "setup"})

	// The body carries actor="client-claim"; the trusted header carries
	// "proxy-user". A passing test proves attributedActor preferred the
	// principal's actor over the body's.
	body := map[string]any{
		"actor":    "client-claim",
		"reason":   "done",
		"message":  "verified end-to-end after the trusted proxy actor fix landed.",
		"evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}},
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"X-Kata-Actor": "proxy-user"})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "close: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload), string(raw))
	require.NotNil(t, payload.Event, "close returned no event row: %s", raw)
	assert.Equal(t, "proxy-user", payload.Event.Actor,
		"event.actor must reflect the trusted header value, not the body actor")
}

// TestTrustedProxyHeader_MissingHeaderRejects verifies the negative path:
// when the trusted listener is configured but the client omits the actor
// header, withTrustedProxyActor installs PrincipalTrustedProxyAbsent and
// ensureAttributedWriteAllowed rejects the write with actor_header_required.
// Without this guard a misconfigured proxy could silently let body-supplied
// actor values through on a listener that callers expect to be header-attributed.
func TestTrustedProxyHeader_MissingHeaderRejects(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	projectID := trustedProxyCreateProject(t, ts, map[string]string{"X-Kata-Actor": "setup"})
	issueRef := trustedProxyCreateIssue(t, ts, projectID,
		map[string]string{"X-Kata-Actor": "setup"})

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "should be rejected because the trusted header is missing.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, nil) // no X-Kata-Actor on the close call
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "actor_header_required")
}

// TestTrustedProxyHeader_ReadsNotBlocked verifies that a header-less GET on a
// trusted listener still returns 2xx. The middleware stores PrincipalTrustedProxyAbsent
// for the read, but read handlers never call attributedActor, so the missing
// header must not reject the request.
func TestTrustedProxyHeader_ReadsNotBlocked(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	// GET /api/v1/health is a read with no actor and no required setup.
	// On a trusted listener with no header, the middleware stores a
	// trusted-but-absent principal; the health handler never calls
	// attributedActor, so it must still return 200.
	resp, raw := doReq(t, ts, "GET", "/api/v1/health", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)
}

// TestTrustedProxyHeader_ModeOffUnchanged verifies that with mode off (empty
// TrustedActorHeader) the header is ignored and the body actor flows through
// as it did before this feature landed. The middleware short-circuits before
// checking the allowlist, so even a request that would otherwise look like a
// trusted-proxy call is attributed to the body actor.
func TestTrustedProxyHeader_ModeOffUnchanged(t *testing.T) {
	// Mode off (empty TrustedActorHeader). The header is ignored, and the
	// body actor is used as today.
	ts := startTrustedProxyTestServer(t, "" /* mode off */)

	projectID := trustedProxyCreateProject(t, ts, nil)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, nil)

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "mode off; body actor must be used.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"X-Kata-Actor": "should-be-ignored"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotNil(t, payload.Event)
	assert.Equal(t, "client-claim", payload.Event.Actor,
		"mode off: header is ignored, body actor wins")
}

// TestTrustedProxyHeader_TokenAdminForbidden locks the cross-mode boundary:
// even on a trusted listener, the trusted-proxy header cannot mint or list
// tokens. The middleware overwrites whatever principal upstream set with
// PrincipalTrustedProxy; ensureTokenAdminAllowed (PR #65) only admits
// PrincipalBootstrap, PrincipalStaticToken, or no-principal, so the request
// is rejected with 403 token_admin_forbidden.
//
// Without this test, a future change to ensureTokenAdminAllowed could silently
// promote PrincipalTrustedProxy into the token-admin allowlist and the only
// signal would be the absence of a test failure.
func TestTrustedProxyHeader_TokenAdminForbidden(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	body := map[string]any{
		"actor": "alice",
		"name":  "test-token",
	}
	resp, raw := doReq(t, ts, "POST", "/api/v1/tokens",
		body, map[string]string{"X-Kata-Actor": "alice"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "body: %s", raw)

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "token_admin_forbidden", env.Error.Code,
		"trusted-proxy principals must not be allowed to mint or revoke tokens")

	// And the same boundary holds for list and revoke.
	resp, raw = doReq(t, ts, "GET", "/api/v1/tokens", nil,
		map[string]string{"X-Kata-Actor": "alice"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "list body: %s", raw)

	resp, raw = doReq(t, ts, "POST", "/api/v1/tokens/42/actions/revoke", nil,
		map[string]string{"X-Kata-Actor": "alice"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "revoke body: %s", raw)
}

// TestTrustedProxyHeader_TokenAdminForbiddenWithoutHeader covers the absent-
// header variant of the same boundary. A request on a trusted listener that
// omits the actor header lands in PrincipalTrustedProxyAbsent, and
// ensureTokenAdminAllowed must reject it just as firmly as the header-present
// case. Without this test, a future change that allowed the absent-header
// principal to bypass token admin (perhaps under the rationale "no actor was
// claimed, so it's safe") would slip through. The header-present test and this
// one together pin both trusted-proxy principal variants out of the token-admin
// allowlist.
func TestTrustedProxyHeader_TokenAdminForbiddenWithoutHeader(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	body := map[string]any{
		"actor": "alice",
		"name":  "test-token",
	}
	resp, raw := doReq(t, ts, "POST", "/api/v1/tokens", body, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "body: %s", raw)

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &env))
	assert.Equal(t, "token_admin_forbidden", env.Error.Code,
		"trusted-proxy absent principals must also be rejected from token admin")

	resp, raw = doReq(t, ts, "GET", "/api/v1/tokens", nil, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "list body: %s", raw)

	resp, raw = doReq(t, ts, "POST", "/api/v1/tokens/42/actions/revoke", nil, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "revoke body: %s", raw)
}

// TestTrustedProxyHeader_TUIBypassRequiresCloseValidation keeps the trusted-
// proxy actor mode aligned with PR #65's token-derived identity rules: once the
// actor is server-derived, a client-controlled source=tui flag must not bypass
// close message/evidence validation.
func TestTrustedProxyHeader_TUIBypassRequiresCloseValidation(t *testing.T) {
	ts := startTrustedProxyTestServer(t, "X-Kata-Actor")

	projectID := trustedProxyCreateProject(t, ts, map[string]string{"X-Kata-Actor": "setup"})
	issueRef := trustedProxyCreateIssue(t, ts, projectID,
		map[string]string{"X-Kata-Actor": "setup"})

	body := map[string]any{
		"actor":  "client-claim",
		"reason": "done",
		"source": "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"X-Kata-Actor": "proxy-user"})
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "validation")
}

// TestTrustedProxyHeader_StaticBearerHeaderWins exercises the simplest
// stacked case: a daemon with both bearer auth (static token) and trusted-
// proxy mode configured. A request that supplies a valid bearer AND the
// trusted-proxy header on the trusted listener must succeed, with the header
// value credited as the actor (not the body actor, and not anything derived
// from the bearer — static tokens have no associated actor).
func TestTrustedProxyHeader_StaticBearerHeaderWins(t *testing.T) {
	ts, _ := startBearerProxyTestServer(t, "X-Kata-Actor",
		bearerProxyOpts{Token: "static-bearer"})
	authed := map[string]string{
		"Authorization": "Bearer static-bearer",
		"X-Kata-Actor":  "proxy-user",
	}

	projectID := trustedProxyCreateProject(t, ts, authed)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, authed)

	body := map[string]any{
		"actor":    "client-claim",
		"reason":   "done",
		"message":  "stacked bearer + proxy must credit the proxy-asserted header value.",
		"evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}},
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, authed)
	require.Equalf(t, http.StatusOK, resp.StatusCode, "close: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotNil(t, payload.Event, "close returned no event: %s", raw)
	assert.Equal(t, "proxy-user", payload.Event.Actor,
		"bearer auth admits the request; trusted-proxy must still overwrite the actor")
}

// TestTrustedProxyHeader_StaticBearerMissingHeaderRejects pins the layered
// error surface: a valid bearer on a trusted listener without the header must
// fail with 400 actor_header_required (not 401 — bearer was fine — and not a
// silent body-actor fallback).
func TestTrustedProxyHeader_StaticBearerMissingHeaderRejects(t *testing.T) {
	ts, _ := startBearerProxyTestServer(t, "X-Kata-Actor",
		bearerProxyOpts{Token: "static-bearer"})
	authed := map[string]string{
		"Authorization": "Bearer static-bearer",
		"X-Kata-Actor":  "setup",
	}

	projectID := trustedProxyCreateProject(t, ts, authed)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, authed)

	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "valid bearer, but no X-Kata-Actor on a trusted listener.",
		"source":  "tui",
	}
	// Same Authorization header, but no X-Kata-Actor.
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{"Authorization": "Bearer static-bearer"})
	assertAPIError(t, resp.StatusCode, raw, http.StatusBadRequest, "actor_header_required")
}

// TestTrustedProxyHeader_BadBearerStops401BeforeProxy verifies the middleware
// ordering at the failure boundary: a request with the trusted-proxy header
// but a wrong bearer must short-circuit at requireBearer with a token error,
// never reaching withTrustedProxyActor. Without this test, swapping the
// middleware order in composition could quietly let the header through on
// unauthenticated requests.
func TestTrustedProxyHeader_BadBearerStops401BeforeProxy(t *testing.T) {
	ts, _ := startBearerProxyTestServer(t, "X-Kata-Actor",
		bearerProxyOpts{Token: "static-bearer"})

	// Setup uses the right bearer so we have something to close.
	authed := map[string]string{
		"Authorization": "Bearer static-bearer",
		"X-Kata-Actor":  "setup",
	}
	projectID := trustedProxyCreateProject(t, ts, authed)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, authed)

	// The close request supplies a bogus bearer plus the proxy header. The
	// header should be ignored because requireBearer rejects first.
	body := map[string]any{
		"actor":   "client-claim",
		"reason":  "done",
		"message": "wrong bearer must reject before the proxy header is considered.",
		"source":  "tui",
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{
			"Authorization": "Bearer wrong-bearer",
			"X-Kata-Actor":  "proxy-user",
		})
	assertAPIError(t, resp.StatusCode, raw, http.StatusForbidden, "auth_invalid")
}

// TestTrustedProxyHeader_DBTokenIdentityHeaderOverwrites exercises the case
// the spec §3.4 precedence table explicitly calls out: identity-mode bearer
// admits the request and PR #65's requireIdentityBearer sets
// PrincipalDBToken{Actor: "bob"}; the trusted-proxy middleware then
// overwrites that principal with PrincipalTrustedProxy{Actor: "alice"} on the
// trusted listener. The resulting event must credit "alice" (header), not
// "bob" (token), not "client-claim" (body). This is the "documented but not
// encouraged" silent override — pinning it here makes any future drift in
// either layer fail loudly.
func TestTrustedProxyHeader_DBTokenIdentityHeaderOverwrites(t *testing.T) {
	ts, store := startBearerProxyTestServer(t, "X-Kata-Actor",
		bearerProxyOpts{Token: "bootstrap-token", RequireTokenIdentity: true})

	// Mint a DB-registered token for "bob" so requireIdentityBearer sets
	// PrincipalDBToken on his requests.
	bobPlain := "bob-db-token"
	_, _, err := store.CreateAPIToken(context.Background(), db.CreateAPITokenParams{
		PlaintextToken: bobPlain,
		Actor:          "bob",
		AdminActor:     db.BootstrapActor,
	})
	require.NoError(t, err)

	// Setup uses the bootstrap token (allowed in identity mode for admin-
	// shaped writes) plus the proxy header for actor attribution.
	setup := map[string]string{
		"Authorization": "Bearer bootstrap-token",
		"X-Kata-Actor":  "setup",
	}
	projectID := trustedProxyCreateProject(t, ts, setup)
	issueRef := trustedProxyCreateIssue(t, ts, projectID, setup)

	// The close uses bob's DB token AND a conflicting header value. Header
	// must win; the DB-token actor "bob" must be silently discarded.
	body := map[string]any{
		"actor":    "client-claim",
		"reason":   "done",
		"message":  "DB-token identity + proxy header: the header must overwrite the DB identity.",
		"evidence": []map[string]any{{"type": "commit", "sha": "abc1234"}},
	}
	resp, raw := doReq(t, ts, "POST",
		"/api/v1/projects/"+strconv.FormatInt(projectID, 10)+"/issues/"+issueRef+"/actions/close",
		body, map[string]string{
			"Authorization": "Bearer " + bobPlain,
			"X-Kata-Actor":  "alice",
		})
	require.Equalf(t, http.StatusOK, resp.StatusCode, "close: %s", raw)

	var payload struct {
		Event *struct {
			Actor string `json:"actor"`
		} `json:"event"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NotNil(t, payload.Event, "close returned no event: %s", raw)
	assert.Equal(t, "alice", payload.Event.Actor,
		"trusted-proxy header must overwrite a DB-token identity on a trusted listener")
}
