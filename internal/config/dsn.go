package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CanonicalDSNIdentity returns a stable, credential-free identity for a database
// DSN, used to namespace per-database runtime state. A bare filesystem path or
// sqlite:// DSN returns the path (the SQLite identity has always been its path).
// A postgres:// DSN returns scheme://host[:port]/db with userinfo and every
// query parameter stripped, so the identity never carries a password and does
// not vary with incidental connection options. The postgres default port (5432)
// is normalized to no-port so the same logical DB referenced with or without
// :5432 produces the same identity. IPv6 hosts are emitted bracketed so the
// result is a valid URL. Malformed DSNs (where url.Parse produces an ambiguous
// host that may embed unencoded credentials) yield a credential-free error.
func CanonicalDSNIdentity(dsn string) (string, error) {
	scheme, rest, hasScheme := splitScheme(dsn)
	if !hasScheme {
		return dsn, nil
	}
	switch scheme {
	case "sqlite":
		return strings.TrimPrefix(rest, "//"), nil
	case "postgres", "postgresql":
		u, err := url.Parse(dsn)
		if err != nil {
			// url.Parse wraps the raw input in its error message — never
			// propagate it; the input may carry credentials.
			return "", errors.New("parse postgres dsn: invalid url")
		}
		if ambiguousUserinfo(u) {
			return "", errors.New("parse postgres dsn: ambiguous credentials (require percent-encoding)")
		}
		host := u.Hostname()
		dbName := strings.TrimPrefix(u.Path, "/")
		port := u.Port()
		if port == "5432" {
			// Postgres default — normalize to no-port so the same logical DB
			// referenced with or without :5432 produces the same identity.
			port = ""
		}
		return "postgres://" + hostPortString(host, port) + "/" + dbName, nil
	default:
		return "", fmt.Errorf("unsupported dsn scheme %q", scheme)
	}
}

// RedactDSN returns dsn with any password removed, safe for errors and logs.
// A scheme-less input (no "://") is treated as a filesystem path and returned
// unchanged; libpq key=value DSNs are not supported and should not be passed
// here. An unparseable or ambiguous DSN returns "" so a malformed string can
// never echo embedded credentials. The query string is dropped entirely —
// postgres URLs can carry credentials there too (e.g. ?password=SECRET,
// ?sslpassword=...), and keeping a maintained allowlist is fragile, so the
// safer default is to redact the whole query for display.
func RedactDSN(dsn string) string {
	if _, _, hasScheme := splitScheme(dsn); !hasScheme {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	if ambiguousUserinfo(u) {
		return ""
	}
	if u.User != nil {
		if _, hasPwd := u.User.Password(); hasPwd {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	u.RawQuery = ""
	return u.String()
}

// ambiguousUserinfo reports whether url.Parse produced the credential-bleed
// shape: u.User is nil but the residual structure shows that an unencoded
// "://" in the password confused the parser. The two shapes:
//   - "@" in u.Host: the userinfo fell into the host segment.
//   - u.Path begins with "//" AND contains "@": the misparsed "://" left a
//     residual leading slash and the credential leaked into the path.
//
// A legitimate "@" in a database path (e.g. "postgres://host/db@tenant")
// yields path "/db@tenant" — single leading slash — which is NOT this shape
// and must canonicalize/redact normally. Treating only the bleed shape as an
// error closes the credential-leak path without rejecting valid @-bearing
// database paths.
func ambiguousUserinfo(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	if strings.Contains(u.Host, "@") {
		return true
	}
	return strings.HasPrefix(u.Path, "//") && strings.Contains(u.Path, "@")
}

// hostPortString emits a postgres canonical host[:port] segment. IPv6 hosts
// are bracketed unconditionally so the output is a valid URL: "[::1]" with
// no port, "[::1]:6543" with a non-default port. IPv4/hostname forms emit
// without brackets.
func hostPortString(host, port string) string {
	if strings.Contains(host, ":") {
		// IPv6: always bracket.
		if port == "" {
			return "[" + host + "]"
		}
		return "[" + host + "]:" + port
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// splitScheme splits "scheme://rest". Reports hasScheme=false for inputs with
// no "://" (bare filesystem paths, including Windows drive paths).
func splitScheme(dsn string) (scheme, rest string, hasScheme bool) {
	i := strings.Index(dsn, "://")
	if i < 0 {
		return "", dsn, false
	}
	return dsn[:i], dsn[i+len("://"):], true
}
