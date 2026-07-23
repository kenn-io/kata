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

func httpOriginPort(u *url.URL) (port string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			port = ""
			err = fmt.Errorf("invalid HTTP origin port: %v", recovered)
		}
	}()
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
