package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"

	"go.kenn.io/kata/internal/httpurl"
)

// PlaneSyncConfig pins daemon-owned API credentials and user-facing links to
// separately configured origins. No credentials are stored in bindings.
type PlaneSyncConfig struct {
	APIOrigin string `toml:"api_origin"`
	WebOrigin string `toml:"web_origin"`
	TokenEnv  string `toml:"token_env"`
}

var planeTokenEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NormalizePlaneSyncConfig validates without resolving secrets or fetching URLs.
func NormalizePlaneSyncConfig(c PlaneSyncConfig) (PlaneSyncConfig, error) {
	c.TokenEnv = strings.TrimSpace(c.TokenEnv)
	if c.TokenEnv == "" {
		c.TokenEnv = "KATA_PLANE_TOKEN"
	}
	if !planeTokenEnvPattern.MatchString(c.TokenEnv) {
		return PlaneSyncConfig{}, errors.New("plane_sync.token_env must be a valid environment variable name")
	}
	if strings.TrimSpace(c.APIOrigin) == "" {
		c.APIOrigin = "https://api.plane.so"
	}
	var err error
	c.APIOrigin, err = CanonicalPlaneOrigin(c.APIOrigin)
	if err != nil {
		return PlaneSyncConfig{}, errors.New("plane_sync.api_origin requires a root HTTPS origin or loopback HTTP origin")
	}
	if strings.TrimSpace(c.WebOrigin) == "" {
		c.WebOrigin = c.APIOrigin
		if c.APIOrigin == "https://api.plane.so" {
			c.WebOrigin = "https://app.plane.so"
		}
	}
	c.WebOrigin, err = CanonicalPlaneOrigin(c.WebOrigin)
	if err != nil {
		return PlaneSyncConfig{}, errors.New("plane_sync.web_origin requires a root HTTPS origin or loopback HTTP origin")
	}
	return c, nil
}

// CanonicalPlaneOrigin rejects path-prefixed origins instead of discarding the
// path, because both API routing and credential pinning depend on this value.
func CanonicalPlaneOrigin(value string) (string, error) {
	value = strings.TrimSpace(value)
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") ||
		strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid Plane origin")
	}
	origin, err := httpurl.CanonicalHTTPOrigin(value)
	if err != nil {
		return "", errors.New("invalid Plane origin")
	}
	canonical, err := url.Parse(origin)
	if err != nil {
		return "", errors.New("invalid Plane origin")
	}
	if canonical.Scheme == "http" {
		ip := net.ParseIP(canonical.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return "", errors.New("plane HTTP origins require a literal loopback address")
		}
	}
	return origin, nil
}
