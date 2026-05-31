package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
)

// ConfigureBearerClient attaches an origin-pinned bearer transport to c. Empty
// token preserves the no-auth client path and skips target safety checks.
func ConfigureBearerClient(c *http.Client, baseURL, token string) error {
	return ConfigureBearerClientWithTrust(c, baseURL, token, resolvedBearerTrustPrivateNetwork())
}

// ConfigureBearerClientWithTrust is ConfigureBearerClient with an explicit
// plaintext-private-network trust decision. Empty token preserves the no-auth
// client path and skips target safety checks.
func ConfigureBearerClientWithTrust(c *http.Client, baseURL, token string, trustPrivateNetwork bool) error {
	if c == nil {
		return fmt.Errorf("nil HTTP client for bearer configuration")
	}
	origin := ""
	var err error
	if token != "" {
		origin, err = BearerOriginForBaseURLWithTrust(baseURL, trustPrivateNetwork)
		if err != nil {
			return err
		}
	}
	c.Transport = BearerTransportWithTrust(c.Transport, token, origin, trustPrivateNetwork)
	return nil
}

func resolvedBearerTrustPrivateNetwork() bool {
	auth, err := ReadAuthConfig()
	if err != nil {
		return EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	}
	return auth.TrustPrivateNetwork
}

// BearerTransport wraps base with bearer-token injection when token is
// non-empty. The token is attached only to requests that still target origin.
func BearerTransport(base http.RoundTripper, token, origin string) http.RoundTripper {
	return BearerTransportWithTrust(base, token, origin, false)
}

// BearerTransportWithTrust wraps base with bearer-token injection when token is
// non-empty, allowing plaintext private-IP targets only when trust is explicit.
func BearerTransportWithTrust(base http.RoundTripper, token, origin string, trustPrivateNetwork bool) http.RoundTripper {
	if token == "" {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{base: base, token: token, origin: origin, trustPrivateNetwork: trustPrivateNetwork}
}

// BearerOriginForBaseURL validates baseURL as a safe bearer target and returns
// the scheme://host origin used for redirect pinning.
func BearerOriginForBaseURL(baseURL string) (string, error) {
	return BearerOriginForBaseURLWithTrust(baseURL, false)
}

// BearerOriginForBaseURLWithTrust validates baseURL as a safe bearer target
// and returns the scheme://host origin used for redirect pinning.
func BearerOriginForBaseURLWithTrust(baseURL string, trustPrivateNetwork bool) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q for bearer-token safety check: %w", baseURL, err)
	}
	if err := CheckBearerTargetSafeURLWithTrust(u, trustPrivateNetwork); err != nil {
		return "", err
	}
	return u.Scheme + "://" + u.Host, nil
}

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
	if err := CheckBearerTargetSafeURLWithTrust(req.URL, t.trustPrivateNetwork); err != nil {
		return nil, err
	}
	if reqOrigin := req.URL.Scheme + "://" + req.URL.Host; reqOrigin != t.origin {
		return nil, fmt.Errorf("refusing to attach bearer token to %q - "+
			"client is bound to daemon origin %q; cross-origin redirects "+
			"are blocked to prevent token leakage", reqOrigin, t.origin)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// CheckBearerTargetSafeURL is the per-request bearer-safety check. Safe
// targets are the Unix-socket sentinel host, HTTPS, and HTTP loopback.
func CheckBearerTargetSafeURL(u *url.URL) error {
	return CheckBearerTargetSafeURLWithTrust(u, false)
}

// CheckBearerTargetSafeURLWithTrust is the per-request bearer-safety check.
// Safe targets are the Unix-socket sentinel host, HTTPS, HTTP loopback, and
// plaintext non-public IP literals only when the operator opted into trusting
// the private network.
func CheckBearerTargetSafeURLWithTrust(u *url.URL, trustPrivateNetwork bool) error {
	if u == nil {
		return fmt.Errorf("nil URL for bearer-token safety check")
	}
	if u.Host == "kata.invalid" {
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
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("plaintext trusted-private-network bearer target %q rejected: address %q is not a literal IP", u.Redacted(), host)
		}
		if ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatBlock.Contains(ip) {
			return nil
		}
		return fmt.Errorf("plaintext trusted-private-network bearer target %q rejected: address %q is not non-public", u.Redacted(), host)
	}
	return fmt.Errorf("refusing to attach bearer token to plaintext non-loopback URL %q - "+
		"the daemon does not terminate TLS, so the token would travel in cleartext; "+
		"use a Unix socket or loopback address, tunnel via SSH, or terminate TLS "+
		"in a reverse proxy", u.Redacted())
}

// cgnatBlock is RFC6598 100.64.0.0/10. Go's net.IP.IsPrivate() intentionally
// excludes it, but private overlay networks commonly use this range.
var cgnatBlock = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}
