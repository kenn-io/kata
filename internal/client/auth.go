package client

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
)

// resolveAuthToken returns the auth token a client should attach to
// outgoing requests. Resolution is:
//
//  1. KATA_AUTH_TOKEN env (highest priority).
//  2. Selected named-daemon token/auth when a workspace selects one.
//  3. [auth].token in <KATA_HOME>/config.toml for the default path.
//  4. Empty (no header injected).
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
	auth, err := resolveAuthConfigForBaseURL("", "")
	if err != nil {
		return config.AuthConfig{
			Token:               strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN")),
			TrustPrivateNetwork: config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK"),
		}
	}
	return auth
}

func resolveAuthConfigForBaseURL(baseURL, workspaceStart string) (config.AuthConfig, error) {
	envToken := strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN"))
	envTrust := config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	auth, target, useNamedAuth, err := baseAuthConfigForTarget(baseURL, workspaceStart, false)
	if err != nil {
		return config.AuthConfig{}, err
	}
	if useNamedAuth && envToken == "" {
		token, err := namedDaemonToken(target.Catalog)
		if err != nil {
			return config.AuthConfig{}, err
		}
		auth.Token = token
	}
	if envToken != "" {
		auth.Token = envToken
	}
	if envTrust {
		auth.TrustPrivateNetwork = true
	}
	return auth, nil
}

func resolveAuthConfigForExplicitBearer(baseURL, workspaceStart, token string) (config.AuthConfig, error) {
	auth, _, _, err := baseAuthConfigForTarget(baseURL, workspaceStart, true)
	if err != nil {
		return config.AuthConfig{}, err
	}
	auth.Token = token
	if config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK") {
		auth.TrustPrivateNetwork = true
	}
	return auth, nil
}

func baseAuthConfigForTarget(
	baseURL, workspaceStart string,
	ignoreNamedTargetErrors bool,
) (config.AuthConfig, namedDaemonTarget, bool, error) {
	auth, err := config.ReadAuthConfig()
	if err != nil {
		auth = config.AuthConfig{}
	}
	target, ok, err := selectedNamedDaemonTarget(workspaceStart)
	if err != nil {
		if ignoreNamedTargetErrors {
			return auth, namedDaemonTarget{}, false, nil
		}
		return config.AuthConfig{}, namedDaemonTarget{}, false, err
	}
	if ok && (baseURL == "" || namedDaemonTargetMatchesBaseURL(target.Catalog, baseURL)) {
		return target.Auth, target, true, nil
	}
	return auth, namedDaemonTarget{}, false, nil
}

func namedDaemonTargetMatchesBaseURL(target config.CatalogDaemonConfig, baseURL string) bool {
	if target.Local {
		return baseURL == UnixBase || strings.HasPrefix(baseURL, UnixBase+"/")
	}
	u, err := normalizeRemoteURL(target.URL, target.AllowInsecure)
	return err == nil && u == strings.TrimRight(baseURL, "/")
}

func namedDaemonToken(target config.CatalogDaemonConfig) (string, error) {
	if target.TokenEnv == "" {
		return strings.TrimSpace(target.Token), nil
	}
	token := strings.TrimSpace(os.Getenv(target.TokenEnv))
	if token == "" {
		return "", fmt.Errorf("daemon %q: token_env %q is unset or empty",
			target.Name, target.TokenEnv)
	}
	return token, nil
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

func authBearerTransport(
	base http.RoundTripper,
	token, baseURL string,
	trustPrivateNetwork bool,
	allowInsecure bool,
) (http.RoundTripper, error) {
	if token == "" {
		return base, nil
	}
	if allowInsecure {
		origin, err := config.BearerOriginForBaseURLAllowInsecure(baseURL)
		if err != nil {
			return nil, err
		}
		return config.BearerTransportWithPolicy(base, token, origin,
			config.BearerPolicy{AllowInsecurePlaintext: true}), nil
	}
	origin, err := checkBearerTargetSafe(baseURL, trustPrivateNetwork)
	if err != nil {
		return nil, err
	}
	return withBearer(base, token, origin, trustPrivateNetwork), nil
}

func explicitBearerTransport(
	base http.RoundTripper,
	token, baseURL string,
	allowInsecure bool,
) (http.RoundTripper, error) {
	return authBearerTransport(base, token, baseURL, false, allowInsecure)
}
