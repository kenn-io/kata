package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAuthTokenEnvWins(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	require.NoError(t, writeAuthConfig(tmp, "from-toml"))
	t.Setenv("KATA_AUTH_TOKEN", "from-env")

	assert.Equal(t, "from-env", resolveAuthToken())
}

func TestResolveAuthTokenFallsBackToTOML(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, writeAuthConfig(tmp, "from-toml"))

	assert.Equal(t, "from-toml", resolveAuthToken())
}

func TestResolveAuthTokenIgnoresUnresolvedCatalogTokenEnv(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "")
	t.Setenv("KATA_WORK_TOKEN", "")
	require.NoError(t, writeRawConfig(tmp, `
[auth]
token = "from-toml"

[[daemon]]
name = "work"
url = "https://kata.example.test"
token_env = "KATA_WORK_TOKEN"
`))

	assert.Equal(t, "from-toml", resolveAuthToken())
}

func TestResolveAuthTokenEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "")

	assert.Empty(t, resolveAuthToken())
}

func TestResolveAuthTokenMalformedTOMLDegradesToEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "")
	require.NoError(t, writeRawConfig(tmp, "not = valid = toml"))

	assert.Empty(t, resolveAuthToken(),
		"malformed config.toml should not panic; degrade to no token")
}

func TestBearerTransportInjectsHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Transport: withBearer(http.DefaultTransport, "secret", srv.URL, false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer secret", got)
}

func TestBearerTransportNoTokenPassthrough(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	rt := withBearer(http.DefaultTransport, "", srv.URL, false)
	assert.Equal(t, http.DefaultTransport, rt,
		"empty token must return the base transport unchanged")

	c := &http.Client{Transport: rt}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Empty(t, got)
}

func TestBearerTransportPreservesCallerHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Transport: withBearer(http.DefaultTransport, "from-transport", srv.URL, false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer caller-supplied")
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer caller-supplied", got,
		"caller-supplied Authorization header must not be overwritten")
}

func TestBearerTransportDoesNotMutateRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Transport: withBearer(http.DefaultTransport, "secret", srv.URL, false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Empty(t, req.Header.Get("Authorization"),
		"the caller's *http.Request must not be mutated by the transport")
}

func TestNewHTTPClientAttachesAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "client-tok")

	c, err := NewHTTPClient(context.Background(), srv.URL, Opts{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer client-tok", got)
}

func TestNewHTTPClientWithBearerAttachesExplicitToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c, err := NewHTTPClientWithBearer(context.Background(), srv.URL, "explicit-token", Opts{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer explicit-token", got)
}

func TestNewHTTPClientForTargetAttachesExplicitTokenOverGlobal(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "global-token")

	c, err := NewHTTPClientForTarget(context.Background(), srv.URL,
		TargetAuth{Token: "target-token"}, Opts{})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/api/v1/projects", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer target-token", got)
}

func TestNewHTTPClientForTargetAllowsBearerForPrivateIPWhenAllowInsecureSet(t *testing.T) {
	c, err := NewHTTPClientForTarget(context.Background(), "http://100.64.0.5:7373",
		TargetAuth{Token: "target-token", AllowInsecure: true}, Opts{})

	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewHTTPClientForTargetRefusesPlaintextHostnameWithAllowInsecure(t *testing.T) {
	_, err := NewHTTPClientForTarget(context.Background(), "http://daemon.internal:7373",
		TargetAuth{Token: "target-token", AllowInsecure: true}, Opts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "literal IP")
}

func TestNewHTTPClientForTargetRefusesPlaintextHostnameWithoutAllowInsecure(t *testing.T) {
	_, err := NewHTTPClientForTarget(context.Background(), "http://daemon.internal:7373",
		TargetAuth{Token: "target-token"}, Opts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

func TestNewHTTPClientForTargetAllowInsecureStillRefusesCrossOrigin(t *testing.T) {
	called := false
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})
	rt, err := explicitBearerTransport(base, "target-token", "http://100.64.0.5:7373", true)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://100.64.0.6:7373/api/v1/ping", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-origin")
	assert.False(t, called, "cross-origin request must not reach the base transport")
}

func TestNewHTTPClientWithBearerEmptyTokenSkipsSafetyCheck(t *testing.T) {
	c, err := NewHTTPClientWithBearer(context.Background(), "http://example.invalid:7373", "", Opts{})

	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewHTTPClientWithBearerRefusesPlaintextNonLoopback(t *testing.T) {
	_, err := NewHTTPClientWithBearer(context.Background(), "http://example.invalid:7373", "secret", Opts{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

func TestNewHTTPClientStreamingAttachesAuthHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "stream-tok")

	c, err := NewHTTPClient(context.Background(), srv.URL, Opts{
		ResponseHeaderTimeout: SSEHandshakeTimeout,
	})
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		srv.URL+"/api/v1/events/stream", nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, "Bearer stream-tok", got,
		"streaming clients must also inject the bearer token")
}

// TestNewHTTPClient_RefusesBearerOnPlaintextNonLoopback verifies the
// defense-in-depth guard in NewHTTPClient: when a token is resolved (env
// or config) and the baseURL is a non-loopback http:// target, client
// construction must fail rather than silently leak the token over the
// wire. The daemon-side checkAuthStartup already refuses to bind in this
// shape, but the client check protects against externally-administered
// daemons and operator misconfiguration.
func TestNewHTTPClient_RefusesBearerOnPlaintextNonLoopback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "")

	_, err := NewHTTPClient(context.Background(), "http://example.invalid:7373", Opts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

func TestNewHTTPClient_AllowsBearerOnPlaintextNonLoopbackWhenTrustPrivateNetworkSet(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	_, err := NewHTTPClient(context.Background(), "http://100.64.0.5:7373", Opts{})
	require.NoError(t, err)
}

func TestNewHTTPClient_TrustPrivateNetworkRejectsPublicPlaintextTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	_, err := NewHTTPClient(context.Background(), "http://8.8.8.8:7373", Opts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-public")
}

func TestNewHTTPClient_TrustPrivateNetworkRejectsPlaintextHostname(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")
	t.Setenv("KATA_TRUST_PRIVATE_NETWORK", "1")

	_, err := NewHTTPClient(context.Background(), "http://example.internal:7373", Opts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "literal IP")
}

// TestNewHTTPClient_AllowsBearerOnLoopback covers the safe-target arm of
// checkBearerTargetSafe: 127.0.0.1 and [::1] keep the token in-host even
// over plaintext HTTP.
func TestNewHTTPClient_AllowsBearerOnLoopback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")

	for _, base := range []string{
		"http://127.0.0.1:7373",
		"http://[::1]:7373",
		"http://localhost:7373",
	} {
		t.Run(base, func(t *testing.T) {
			_, err := NewHTTPClient(context.Background(), base, Opts{})
			require.NoError(t, err)
		})
	}
}

// TestNewHTTPClient_AllowsBearerOverHTTPS covers the TLS-protected arm:
// an https:// baseURL is safe to attach a token to regardless of host.
func TestNewHTTPClient_AllowsBearerOverHTTPS(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "secret")

	_, err := NewHTTPClient(context.Background(), "https://example.invalid", Opts{})
	require.NoError(t, err)
}

// TestCheckBearerTargetSafe_UnixBase verifies the UnixBase sentinel URL
// passes the safety check directly. The previous test went through
// NewHTTPClient, which can fail in unixClientFromRuntime before reaching
// the bearer-attachment code path — so a regression in the UnixBase
// branch of checkBearerTargetSafe would have been masked. Hitting the
// helper directly makes the coverage unambiguous.
//
// The returned origin is the scheme+host stripped of any path component —
// path-bearing baseURLs must normalize to the same origin so the per-request
// pin matches regardless of which API path triggered the check.
func TestCheckBearerTargetSafe_UnixBase(t *testing.T) {
	origin, err := checkBearerTargetSafe(UnixBase, false)
	require.NoError(t, err)
	assert.Equal(t, UnixBase, origin)

	origin, err = checkBearerTargetSafe(UnixBase+"/api/v1/ping", false)
	require.NoError(t, err)
	assert.Equal(t, UnixBase, origin)
}

// TestNewHTTPClient_NoTokenSkipsSafetyCheck verifies the gate is
// token-conditional: when KATA_AUTH_TOKEN is unset, even non-loopback
// plaintext URLs are fine because no token will be attached.
func TestNewHTTPClient_NoTokenSkipsSafetyCheck(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KATA_HOME", tmp)
	t.Setenv("KATA_AUTH_TOKEN", "")

	_, err := NewHTTPClient(context.Background(), "http://example.invalid:7373", Opts{})
	require.NoError(t, err)
}

// TestBearerTransport_RefusesPlaintextNonLoopbackPerRequest pins the
// per-request guard added to bearerTransport.RoundTrip: even if the
// client was constructed against a safe base URL, an individual request
// pointed at a plaintext non-loopback URL must be refused before the
// token reaches the wire.
func TestBearerTransport_RefusesPlaintextNonLoopbackPerRequest(t *testing.T) {
	rt := withBearer(http.DefaultTransport, "secret", "http://127.0.0.1:7373", false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://example.invalid:7373/api/v1/ping", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

func TestBearerTransport_AllowsPlaintextNonLoopbackWhenTrustPrivateNetworkSet(t *testing.T) {
	var got string
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})
	rt := withBearer(base, "secret", "http://100.64.0.5:7373", true)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://100.64.0.5:7373/api/v1/ping", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, "Bearer secret", got)
}

func TestBearerTransport_TrustPrivateNetworkStillRefusesCrossOrigin(t *testing.T) {
	called := false
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})
	rt := withBearer(base, "secret", "http://100.64.0.5:7373", true)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://100.64.0.6:7373/api/v1/ping", nil)
	require.NoError(t, err)

	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-origin")
	assert.False(t, called, "cross-origin request must not reach the base transport")
}

// TestBearerTransport_BlocksTokenOnRedirectToPlaintextNonLoopback is the
// concrete regression test for the per-request guard. The test server
// 302-redirects to a plaintext non-loopback URL; http.Client follows the
// redirect through the same transport, and the redirected RoundTrip call
// must refuse to attach the bearer rather than leaking it in cleartext.
func TestBearerTransport_BlocksTokenOnRedirectToPlaintextNonLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid:7373/api/v1/ping", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := &http.Client{Transport: withBearer(http.DefaultTransport, "secret", srv.URL, false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plaintext")
}

// TestBearerTransport_RefusesCrossOriginPerRequest pins the origin-binding
// guard in bearerTransport.RoundTrip directly: the transport is constructed
// against one origin, and a request to any other origin (even one that
// passes the HTTPS / loopback safety check on its own) must be refused
// before the token reaches the wire.
func TestBearerTransport_RefusesCrossOriginPerRequest(t *testing.T) {
	rt := withBearer(http.DefaultTransport, "secret", "https://daemon.example", false)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://attacker.example/api/v1/ping", nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-origin")
	assert.Contains(t, err.Error(), "attacker.example")
	assert.Contains(t, err.Error(), "daemon.example")
}

// TestBearerTransport_BlocksTokenOnCrossOriginHTTPSRedirect is the
// regression test for the finding the HTTPS-only safety check missed:
// the trusted daemon (or an attacker-influenced base URL) 302-redirects
// to https://attacker.example. checkBearerTargetSafeURL accepts both
// hosts as "safe transport," so without origin pinning the redirected
// request would carry Authorization: Bearer <token> to the attacker.
// With origin pinning, the redirected RoundTrip refuses and never
// dials the attacker host at all.
func TestBearerTransport_BlocksTokenOnCrossOriginHTTPSRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://attacker.example/api/v1/ping", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	base := srv.Client().Transport
	c := &http.Client{Transport: withBearer(base, "secret", srv.URL, false)}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := c.Do(req) //nolint:gosec // G704: srv.URL is the test's own httptest.Server
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cross-origin",
		"redirect to a different HTTPS origin must be refused, not silently followed with bearer")
	assert.Contains(t, err.Error(), "attacker.example")
}

// writeAuthConfig writes a config.toml with [auth].token = tok under home.
func writeAuthConfig(home, tok string) error {
	return writeRawConfig(home, "[auth]\ntoken = \""+tok+"\"\n")
}

// writeRawConfig writes config.toml verbatim under home.
func writeRawConfig(home, body string) error {
	return os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
