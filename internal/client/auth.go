package client

import (
	"net/http"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
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
	auth, err := config.ReadAuthConfig()
	if err != nil {
		return config.AuthConfig{Token: envToken, TrustPrivateNetwork: envTrust}
	}
	return auth
}

// withBearer wraps base with bearer-token injection when token is
// non-empty. When token is empty the base transport is returned
// unchanged so the no-auth daemon deployments incur zero extra cost.
// A nil base falls back to http.DefaultTransport — matching net/http's
// own zero-value behavior when *http.Client.Transport is nil. origin
// is the scheme://host the bearer is pinned to (see bearerTransport).
func withBearer(base http.RoundTripper, token, origin string, trustPrivateNetwork bool) http.RoundTripper {
	return config.BearerTransportWithTrust(base, token, origin, trustPrivateNetwork)
}

// checkBearerTargetSafe refuses to attach a bearer token to a baseURL that
// would put the token on the wire in cleartext, and returns the scheme://host
// origin the bearer should be pinned to for subsequent requests. Thin wrapper
// over checkBearerTargetSafeURL that accepts a string base URL — used at
// client construction time to fail fast before any request is built.
func checkBearerTargetSafe(baseURL string, trustPrivateNetwork bool) (string, error) {
	return config.BearerOriginForBaseURLWithTrust(baseURL, trustPrivateNetwork)
}

func explicitBearerTransport(
	base http.RoundTripper,
	token, baseURL string,
	allowInsecure bool,
) (http.RoundTripper, error) {
	if token == "" {
		return base, nil
	}
	origin, err := checkBearerTargetSafe(baseURL, allowInsecure)
	if err != nil {
		return nil, err
	}
	return withBearer(base, token, origin, allowInsecure), nil
}
