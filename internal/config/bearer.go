package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
)

// ConfigureClient attaches an origin-pinned bearer transport to c under this
// policy.
//
// The target is validated whether or not a token is present (decision D2,
// daemon-strict). An empty token against a safe target installs nothing and
// returns nil; an unsafe target is an error regardless of token presence, so a
// misrouted or plaintext-exposed endpoint is reported at construction instead
// of silently producing a client that will fail later as an opaque 401. Legal
// deployments stay legal through TrustPrivateNetwork and
// AllowInsecurePlaintext.
func (p BearerPolicy) ConfigureClient(c *http.Client, baseURL, token string) error {
	if c == nil {
		return fmt.Errorf("nil HTTP client for bearer configuration")
	}
	origin, err := p.OriginForBaseURL(baseURL)
	if err != nil {
		return err
	}
	c.Transport = p.Transport(c.Transport, token, origin)
	return nil
}

// ResolvedBearerTrustPrivateNetwork reports the operator's configured
// private-network trust decision ([auth].trust_private_network, or
// KATA_TRUST_PRIVATE_NETWORK when the config cannot be read).
func ResolvedBearerTrustPrivateNetwork() bool {
	auth, err := ReadAuthConfig()
	if err != nil {
		return EnvTruthy("KATA_TRUST_PRIVATE_NETWORK")
	}
	return auth.TrustPrivateNetwork
}

// BearerPolicy controls bearer-token target validation.
type BearerPolicy struct {
	// TrustPrivateNetwork allows plaintext HTTP only for literal non-public
	// IPs. It is intended for daemon-wide private-network opt-in.
	TrustPrivateNetwork bool
	// AllowInsecurePlaintext skips the plaintext target safety check. Origin
	// pinning still applies, so cross-origin redirects cannot receive tokens.
	AllowInsecurePlaintext bool
}

// CheckTargetURL reports whether u is a safe target for a bearer token under
// this policy. It is the single definition of that rule: the per-request guard
// in bearerTransport.RoundTrip, construction-time validation in
// ConfigureClient, and OriginForBaseURL all go through here.
//
// AllowInsecurePlaintext skips the plaintext-exposure judgement but NOT the
// shape rules — an unparseable scheme or a missing host is still refused, and
// origin pinning (applied by the transport) still prevents a token following a
// cross-origin redirect. That combination is what makes an explicit
// allow_insecure target safe to pin a token to.
func (p BearerPolicy) CheckTargetURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("nil URL for bearer-token safety check")
	}
	if u.Host == "kata.invalid" {
		return nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q for bearer-token client", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("bearer-token target %q must include host", u.Redacted())
	}
	if p.AllowInsecurePlaintext || u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if host == "" || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	if p.TrustPrivateNetwork {
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
		"use a Unix socket or loopback address, tunnel via SSH, terminate TLS "+
		"in a reverse proxy, or opt into the private network with "+
		"KATA_TRUST_PRIVATE_NETWORK=1 ([auth].trust_private_network)", u.Redacted())
}

// OriginForBaseURL validates baseURL under this policy and returns the
// canonical scheme://host origin the bearer is pinned to.
func (p BearerPolicy) OriginForBaseURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base URL %q for bearer-token safety check: %w", baseURL, err)
	}
	if err := p.CheckTargetURL(u); err != nil {
		return "", err
	}
	return CanonicalHTTPOrigin(baseURL)
}

// Transport wraps base with bearer-token injection when token is non-empty,
// applying this policy per request. An empty token returns base unchanged so
// no-auth deployments incur zero cost; target validation for the empty-token
// case belongs to ConfigureClient, not here.
func (p BearerPolicy) Transport(base http.RoundTripper, token, origin string) http.RoundTripper {
	if token == "" {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{base: base, token: token, origin: origin, policy: p}
}

type bearerTransport struct {
	base   http.RoundTripper
	token  string
	origin string
	policy BearerPolicy
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.token == "" || req.Header.Get("Authorization") != "" {
		return t.base.RoundTrip(req)
	}
	if err := t.policy.CheckTargetURL(req.URL); err != nil {
		return nil, err
	}
	reqOrigin, err := CanonicalHTTPOrigin(req.URL.String())
	if err != nil {
		return nil, fmt.Errorf("canonicalize bearer request origin: %w", err)
	}
	boundOrigin, err := CanonicalHTTPOrigin(t.origin)
	if err != nil {
		return nil, fmt.Errorf("canonicalize bound bearer origin: %w", err)
	}
	if reqOrigin != boundOrigin {
		return nil, fmt.Errorf("refusing to attach bearer token to %q - "+
			"client is bound to daemon origin %q; cross-origin redirects "+
			"are blocked to prevent token leakage", reqOrigin, boundOrigin)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(clone)
}

// cgnatBlock is RFC6598 100.64.0.0/10. Go's net.IP.IsPrivate() intentionally
// excludes it, but private overlay networks commonly use this range.
var cgnatBlock = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}
