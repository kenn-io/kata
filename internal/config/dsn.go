package config

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
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

// PostgresTarget is one host and port that a PostgreSQL DSN can connect to.
type PostgresTarget struct {
	Host string
	Port uint16
}

// PostgresTargetParser returns the database and the connection targets,
// primary first, that the PostgreSQL driver selects for dsn.
type PostgresTargetParser func(dsn string) (database string, targets []PostgresTarget, err error)

var postgresTargetParser PostgresTargetParser

// UsePostgresTargetParser installs the driver-backed parser that resolves
// PostgreSQL routing options. The PostgreSQL storage package installs it, so
// every program that can open PostgreSQL storage has it. Programs without it
// still identify DSNs whose URL alone selects the target.
func UsePostgresTargetParser(parser PostgresTargetParser) {
	postgresTargetParser = parser
}

// ErrPostgresRoutingUnavailable reports a PostgreSQL DSN whose targets depend
// on routing options or PG* environment settings that this program cannot
// resolve because it does not include the PostgreSQL driver.
var ErrPostgresRoutingUnavailable = errors.New("resolve PostgreSQL routing options in the storage DSN: this program does not include the PostgreSQL driver")

// postgresRoutingQueryKeys are the URL query options that let the driver
// choose a host, port, or database other than the URL's authority and path.
var postgresRoutingQueryKeys = []string{"host", "port", "dbname", "database", "service", "servicefile"}

type postgresConnectionTarget struct {
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

type postgresConnectionTargetIdentity struct {
	Database string                     `json:"database"`
	Targets  []postgresConnectionTarget `json:"targets"`
}

// postgresTargetIdentity returns the effective credential-free PostgreSQL
// connection targets selected by the PostgreSQL driver. Unlike CanonicalDSNIdentity, which is a
// display-safe database label, this identity retains routing overrides and HA
// fallbacks so two different servers cannot share daemon runtime state.
func postgresTargetIdentity(dsn string) (string, error) {
	canonical, err := CanonicalDSNIdentity(dsn)
	if err != nil {
		return "", err
	}
	ambientRouting, err := postgresTargetUsesAmbientRouting(dsn)
	if err != nil {
		return "", err
	}
	if !ambientRouting && !postgresURLHasRoutingOptions(dsn) {
		// The URL's authority and path alone select the targets, so the driver
		// would report the canonical targets. Skipping the driver lets
		// client-only programs find the same runtime namespace as the daemon.
		return canonical, nil
	}
	identity, err := parsePostgresConnectionTargetIdentity(dsn)
	if err != nil {
		return "", err
	}
	if !ambientRouting {
		canonicalIdentity, err := parsePostgresCanonicalTargetIdentity(dsn)
		if err != nil {
			return "", err
		}
		if samePostgresConnectionTargetIdentity(identity, canonicalIdentity) {
			// Preserve the original runtime namespace for ordinary DSNs. Only
			// routing overrides that change the driver's effective targets need the new
			// expanded identity.
			return canonical, nil
		}
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return "", errors.New("encode postgres target identity")
	}
	return string(body), nil
}

// postgresTargetUsesAmbientRouting reports whether the driver can fill a missing URL
// target field from libpq environment settings. Such targets must retain the
// expanded effective identity: the same URL can otherwise address different
// servers in two shells while sharing one daemon namespace.
func postgresTargetUsesAmbientRouting(dsn string) (bool, error) {
	u, err := url.Parse(dsn)
	if err != nil || ambiguousUserinfo(u) {
		return false, errors.New("parse postgres target identity: invalid dsn")
	}
	query := u.Query()
	hostExplicit := u.Hostname() != "" || query.Has("host")
	portExplicit := u.Port() != "" || query.Has("port")
	databaseExplicit := strings.TrimLeft(u.Path, "/") != "" || query.Has("database") || query.Has("dbname")
	if (os.Getenv("PGSERVICE") != "" || query.Has("service")) &&
		(!hostExplicit || !portExplicit || !databaseExplicit) {
		return true, nil
	}
	return os.Getenv("PGHOST") != "" && !hostExplicit ||
		os.Getenv("PGPORT") != "" && !portExplicit ||
		os.Getenv("PGDATABASE") != "" && !databaseExplicit, nil
}

// postgresURLHasRoutingOptions reports whether dsn's query overrides the
// targets that its authority and path select.
func postgresURLHasRoutingOptions(dsn string) bool {
	u, err := url.Parse(dsn)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(postgresRoutingQueryKeys, u.Query().Has)
}

// parsePostgresCanonicalTargetIdentity derives the ordinary URL target from
// the original parsed DSN while removing credentials and query overrides. It
// must not reparse CanonicalDSNIdentity: that legacy display identity contains
// decoded database-path characters, so a database name containing an encoded
// '#' or '?' would be reinterpreted as URL syntax and split the runtime
// namespace after an upgrade.
func parsePostgresCanonicalTargetIdentity(dsn string) (postgresConnectionTargetIdentity, error) {
	u, err := url.Parse(dsn)
	if err != nil || ambiguousUserinfo(u) {
		return postgresConnectionTargetIdentity{}, errors.New("parse postgres target identity: invalid dsn")
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return parsePostgresConnectionTargetIdentity(u.String())
}

func parsePostgresConnectionTargetIdentity(dsn string) (postgresConnectionTargetIdentity, error) {
	if postgresTargetParser == nil {
		return postgresConnectionTargetIdentity{}, ErrPostgresRoutingUnavailable
	}
	database, targets, err := postgresTargetParser(dsn)
	if err != nil {
		// Driver parse errors may echo credentials from the input.
		return postgresConnectionTargetIdentity{}, errors.New("parse postgres target identity: invalid dsn")
	}
	identity := postgresConnectionTargetIdentity{Database: database}
	seen := map[postgresConnectionTarget]struct{}{}
	appendTarget := func(host string, port uint16) {
		target := postgresConnectionTarget{Host: host, Port: port}
		if _, exists := seen[target]; exists {
			return
		}
		seen[target] = struct{}{}
		identity.Targets = append(identity.Targets, target)
	}
	for _, target := range targets {
		appendTarget(target.Host, target.Port)
	}
	return identity, nil
}

func samePostgresConnectionTargetIdentity(a, b postgresConnectionTargetIdentity) bool {
	if a.Database != b.Database || len(a.Targets) != len(b.Targets) {
		return false
	}
	for index := range a.Targets {
		if a.Targets[index] != b.Targets[index] {
			return false
		}
	}
	return true
}

// RedactDSN returns dsn with any password removed, safe for errors and logs.
// A scheme-less input (no "://") is normally treated as a filesystem path and
// returned unchanged, but libpq key=value DSNs carrying password fields are
// replaced with a fixed placeholder. An unparseable or ambiguous DSN returns
// "" so a malformed string can never echo embedded credentials. The query
// string is dropped entirely — postgres URLs can carry credentials there too
// (e.g. ?password=SECRET, ?sslpassword=...), and keeping a maintained allowlist
// is fragile, so the safer default is to redact the whole query for display.
func RedactDSN(dsn string) string {
	if _, _, hasScheme := splitScheme(dsn); !hasScheme {
		if hasLibpqKeywordCredential(dsn) {
			return "<redacted libpq keyword dsn>"
		}
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
	before, after, ok := strings.Cut(dsn, "://")
	if !ok {
		return "", dsn, false
	}
	return before, after, true
}

var libpqKeywordParams = map[string]struct{}{
	"application_name":     {},
	"connect_timeout":      {},
	"dbname":               {},
	"host":                 {},
	"hostaddr":             {},
	"keepalives":           {},
	"keepalives_count":     {},
	"keepalives_idle":      {},
	"keepalives_interval":  {},
	"passfile":             {},
	"password":             {},
	"port":                 {},
	"sslcert":              {},
	"sslkey":               {},
	"sslmode":              {},
	"sslpassword":          {},
	"sslrootcert":          {},
	"target_session_attrs": {},
	"user":                 {},
}

var libpqKeywordPattern = regexp.MustCompile(`(?i)(?:^|[[:space:]])(application_name|connect_timeout|dbname|host|hostaddr|keepalives|keepalives_count|keepalives_idle|keepalives_interval|passfile|password|port|sslcert|sslkey|sslmode|sslpassword|sslrootcert|target_session_attrs|user)[[:space:]]*=`)

var libpqCredentialPattern = regexp.MustCompile(`(?i)(?:^|[[:space:]])(password|sslpassword)[[:space:]]*=`)

func firstLibpqKeywordParam(dsn string) (string, bool) {
	match := libpqKeywordPattern.FindStringSubmatch(dsn)
	if len(match) < 2 {
		return "", false
	}
	key := strings.ToLower(match[1])
	_, ok := libpqKeywordParams[key]
	return key, ok
}

func hasLibpqKeywordCredential(dsn string) bool {
	return libpqCredentialPattern.MatchString(dsn)
}
