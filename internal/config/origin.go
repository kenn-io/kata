package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// CanonicalHTTPOrigin parses raw as an HTTP(S) URL and returns its canonical
// scheme://host[:port] origin. Paths, queries, and fragments are discarded.
func CanonicalHTTPOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse HTTP origin: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("HTTP origin scheme must be http or https, got %q", u.Scheme)
	}
	if u.Hostname() == "" || u.User != nil {
		return "", fmt.Errorf("HTTP origin must include a host and no user info")
	}
	host, zone, hasZone := strings.Cut(u.Hostname(), "%")
	host = strings.ToLower(host)
	if hasZone {
		host += "%" + zone
	}
	port, err := httpOriginPort(u)
	if err != nil {
		return "", err
	}
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host = net.JoinHostPort(strings.Trim(host, "[]"), port)
	}
	return (&url.URL{Scheme: scheme, Host: host}).String(), nil
}

// CanonicalHTTPBaseURL returns a canonical HTTP(S) origin plus its configured
// reverse-proxy path prefix. Query, fragment, and user-info components are
// rejected because callers append fixed API paths and pin credentials to this
// exact base.
func CanonicalHTTPBaseURL(raw string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("parse HTTP base URL: %w", err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("HTTP base URL must not include query or fragment")
	}
	origin, err := CanonicalHTTPOrigin(trimmed)
	if err != nil {
		return "", err
	}
	base, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("parse canonical HTTP origin: %w", err)
	}
	base.Path = u.Path
	base.RawPath = u.RawPath
	return base.String(), nil
}

// EffectiveHTTPAllowInsecure normalizes transport policy for a concrete URL.
// HTTPS never needs the plaintext opt-in, even when the source configuration
// redundantly sets it.
func EffectiveHTTPAllowInsecure(raw string, configured bool) (bool, error) {
	base, err := CanonicalHTTPBaseURL(raw)
	if err != nil {
		return false, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return false, err
	}
	return configured && u.Scheme == "http", nil
}

func httpOriginPort(u *url.URL) (string, error) {
	raw := u.Port()
	if raw == "" {
		return "", nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > 65535 {
		return "", fmt.Errorf("invalid HTTP origin port %q", raw)
	}
	return strconv.Itoa(value), nil
}
