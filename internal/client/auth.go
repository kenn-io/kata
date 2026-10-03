package client

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"go.kenn.io/kata/internal/config"
)

// resolveAuthToken returns the auth token a client should attach to
// outgoing requests. Resolution mirrors the daemon side:
//
//  1. KATA_AUTH_TOKEN env (highest priority).
//  2. [auth].token > token_file > token_env in <KATA_HOME>/config.toml.
//  3. Empty (no header injected).
//
// Unrelated TOML errors retain the legacy environment-only fallback. Selected
// credential errors are retained and stop HTTP client construction.
func resolveAuthToken() string {
	return resolveAuthConfig().Token
}

func resolveAuthConfig() config.AuthConfig {
	envToken := authTokenEnvOverride()
	envTrust := config.EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	auth, err := config.ReadAuthConfig()
	if err != nil {
		if errors.Is(err, config.ErrCredentialSource) {
			auth.CredentialError = err
			return auth
		}
		// A malformed TOML file must not erase an independently selected
		// environment file source. Keep the legacy environment-only fallback.
		token, _, credentialErr := config.ResolveSecret(envToken, os.Getenv("KATA_AUTH_TOKEN_FILE"), "")
		return config.AuthConfig{Token: token, TrustPrivateNetwork: envTrust, CredentialError: credentialErr}
	}
	return auth
}

func authTokenEnvOverride() string {
	return strings.TrimSpace(os.Getenv("KATA_AUTH_TOKEN"))
}

// bearerPolicyFor builds the target-validation policy for a client.
// allow_insecure is an explicit per-target operator opt-out and subsumes the
// private-network trust axis, so the two are never both set.
func bearerPolicyFor(trustPrivateNetwork, allowInsecure bool) config.BearerPolicy {
	if allowInsecure {
		return config.BearerPolicy{AllowInsecurePlaintext: true}
	}
	return config.BearerPolicy{TrustPrivateNetwork: trustPrivateNetwork}
}

// authBearerTransport wraps base with an origin-pinned bearer transport for
// baseURL. The target is validated whether or not token is empty (decision
// D2): an empty token against a safe target installs nothing, while an unsafe
// target is an error either way.
func authBearerTransport(
	base http.RoundTripper,
	token, baseURL string,
	trustPrivateNetwork bool,
	allowInsecure bool,
) (http.RoundTripper, error) {
	policy := bearerPolicyFor(trustPrivateNetwork, allowInsecure)
	origin, err := policy.OriginForBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	return policy.Transport(base, token, origin), nil
}
