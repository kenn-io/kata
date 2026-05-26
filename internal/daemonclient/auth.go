package daemonclient

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

// resolveAuthToken returns the auth token a client should attach to
// outgoing requests. Resolution mirrors the daemon side:
//
//  1. KATA_AUTH_TOKEN env (highest priority).
//  2. [auth].token in <KATA_HOME>/config.toml.
//  3. Empty (no header injected).
//
// Errors reading the TOML are not surfaced: a misformatted file should
// not silently strand the CLI on a no-auth path, but it also should not
// block discovery. Daemon startup (which always calls ReadDaemonConfig)
// is the surface that reports parse errors loudly. Here we degrade to
// "no token" so the request fails with a clean 401 rather than a noisy
// client-side decode error.
func resolveAuthToken() string {
	return resolveAuthConfig().Token
}

func resolveAuthConfig() config.AuthConfig {
	envToken := strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN"))
	envTrust := config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	cfg, err := config.ReadDaemonConfig()
	if err != nil || cfg == nil {
		return config.AuthConfig{Token: envToken, TrustPrivateNetwork: envTrust}
	}
	return cfg.Auth
}

// bearerTransport wraps an http.RoundTripper and injects
// Authorization: Bearer <token> on every outgoing request unless the
// caller already supplied an Authorization header. Cloning the request
// keeps the upstream caller's *http.Request untouched, which matters
// when the same request is replayed (e.g. retry loops or SSE
// reconnects that recycle a parent request object).
//
// The /api/v1/ping and /api/v1/health endpoints do not require auth,
// but the daemon's middleware ignores the header on those paths. We
// inject unconditionally so a single transport works for discovery
// probes, normal API calls, and SSE streams alike.
//
// origin is the scheme://host captured at client construction time. The
// token is attached only when req.URL still targets that origin; redirects
// to any other origin lose the token. checkBearerTargetSafeURL alone
// accepts any https:// URL as safe, so without origin pinning a daemon
// (or attacker-influenced discovery / base URL) redirecting to
// https://attacker.example would leak the bearer over the wire even
// though the redirected request "looks safe" by transport-encryption
// alone.
type bearerTransport struct {
	base                http.RoundTripper
	token               string
	origin              string
	trustPrivateNetwork bool
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token == "" || req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	// Per-request safety check: a baseURL-only check at construction can be
	// bypassed if the server redirects from a safe target (HTTPS / loopback)
	// to a plaintext non-loopback URL — http.Client follows the redirect via
	// the same transport, and we would attach the token to the redirected
	// request. Re-validating req.URL here covers both initial requests and
	// follow-up redirects without trusting the client redirect policy.
	if err := checkBearerTargetSafeURL(req.URL, t.trustPrivateNetwork); err != nil {
		return nil, err
	}
	if reqOrigin := req.URL.Scheme + "://" + req.URL.Host; reqOrigin != t.origin {
		return nil, fmt.Errorf("refusing to attach bearer token to %q — "+
			"client is bound to daemon origin %q; cross-origin redirects "+
			"are blocked to prevent token leakage", reqOrigin, t.origin)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// withBearer wraps base with bearer-token injection when token is
// non-empty. When token is empty the base transport is returned
// unchanged so the no-auth daemon deployments incur zero extra cost.
// A nil base falls back to http.DefaultTransport — matching net/http's
// own zero-value behavior when *http.Client.Transport is nil. origin
// is the scheme://host the bearer is pinned to (see bearerTransport).
func withBearer(base http.RoundTripper, token, origin string, trustPrivateNetwork bool) http.RoundTripper {
	if token == "" {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{
		base:                base,
		token:               token,
		origin:              origin,
		trustPrivateNetwork: trustPrivateNetwork,
	}
}

// checkBearerTargetSafe refuses to attach a bearer token to a baseURL that
// would put the token on the wire in cleartext, and returns the scheme://host
// origin the bearer should be pinned to for subsequent requests. Thin wrapper
// over checkBearerTargetSafeURL that accepts a string base URL — used at
// client construction time to fail fast before any request is built.
func checkBearerTargetSafe(baseURL string, trustPrivateNetwork bool) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q for bearer-token safety check: %w", baseURL, err)
	}
	if err := checkBearerTargetSafeURL(u, trustPrivateNetwork); err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

// unixSentinelHost is the host portion of UnixBase — the synthetic value
// http.Request sees when daemonclient dials a Unix socket. Treated as safe
// because the request never leaves the host.
const unixSentinelHost = "kata.invalid"

// checkBearerTargetSafeURL is the per-request form of the bearer-safety check.
// Safe targets are the Unix-socket sentinel host, HTTPS schemes, HTTP loopback
// addresses (including "localhost", "127.0.0.1", "[::1]"), and plaintext
// private-IP URLs only when the operator opted into private-network trust.
// Defense in depth on top of checkAuthStartup ensures a redirect-following
// client cannot leak the token to an untrusted origin.
func checkBearerTargetSafeURL(u *url.URL, trustPrivateNetwork bool) error {
	if u == nil {
		return fmt.Errorf("nil URL for bearer-token safety check")
	}
	if u.Host == unixSentinelHost {
		return nil
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return fmt.Errorf("unsupported URL scheme %q for bearer-token client", u.Scheme)
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if trustPrivateNetwork {
		if err := daemon.ValidateNonPublicAddress(net.JoinHostPort(host, u.Port())); err != nil {
			return fmt.Errorf("plaintext trusted-private-network bearer target %q rejected: %w", u.Redacted(), err)
		}
		return nil
	}
	return fmt.Errorf("refusing to attach bearer token to plaintext non-loopback URL %q — "+
		"the daemon does not terminate TLS, so the token would travel in cleartext; "+
		"use a Unix socket or loopback address, tunnel via SSH, or terminate TLS "+
		"in a reverse proxy", u.Redacted())
}
