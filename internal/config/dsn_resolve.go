package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KataDSN returns the effective database DSN honoring (in precedence order)
// $KATA_DSN, $KATA_DB (legacy env override), [storage].dsn from
// <KATA_HOME>/config.toml, and the <KATA_HOME>/kata.db default. The returned
// string is whatever the user supplied: a bare path, sqlite:// DSN, or
// postgres:// DSN. Callers pass it directly to storeopen.Open. Shape
// validation rejects unknown schemes and libpq query params on sqlite/bare
// DSNs with credential-free errors; validation is shape-only and never dials.
//
// Env vars take precedence over the config file so a user's existing shell
// (with KATA_DB exported) keeps pointing at the same database after the
// config-file knob lands — without this ordering, an absent-from-the-shell
// [storage].dsn would silently redirect long-running scripts to a different
// DB the next time they re-resolved.
//
// The TOML branch reads only the [storage] section via readStorageConfig so
// a parse error in an unrelated section (auth, listen, close, ...) does not
// block legacy KATA_DB callers that never cared about the daemon config.
//
// ctx is accepted for symmetry with other resolver-style helpers and to let
// future implementations short-circuit a slow read; the current body does
// not dispatch on it.
func KataDSN(ctx context.Context) (string, error) {
	_ = ctx
	if v := strings.TrimSpace(os.Getenv("KATA_DSN")); v != "" {
		if err := validateDSN(v); err != nil {
			return "", err
		}
		return v, nil
	}
	if v := strings.TrimSpace(os.Getenv("KATA_DB")); v != "" {
		if err := validateDSN(v); err != nil {
			return "", err
		}
		return v, nil
	}
	storage, err := readStorageConfig()
	if err != nil {
		return "", err
	}
	if v := storage.DSN; v != "" {
		if err := validateDSN(v); err != nil {
			return "", err
		}
		return v, nil
	}
	home, err := KataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "kata.db"), nil
}

// validateDSN performs shape-only validation: it rejects unknown schemes,
// scheme-less libpq keyword DSNs, and libpq query params on sqlite/bare DSNs,
// and propagates the ambiguous-credentials probe from CanonicalDSNIdentity for
// postgres DSNs. It never echoes the DSN itself in errors — credentials would
// leak through error logs otherwise.
func validateDSN(dsn string) error {
	scheme, _, hasScheme := splitScheme(dsn)
	switch {
	case hasScheme && scheme != "sqlite" && scheme != "postgres" && scheme != "postgresql":
		return fmt.Errorf("unsupported dsn scheme %q", scheme)
	case hasScheme && (scheme == "postgres" || scheme == "postgresql"):
		// Probe for the credential-bleed shape so the error stays
		// credential-free. CanonicalDSNIdentity already implements the probe
		// and never echoes the input on error.
		if _, err := CanonicalDSNIdentity(dsn); err != nil {
			return err
		}
		return nil
	}
	if !hasScheme {
		if param, ok := firstLibpqKeywordParam(dsn); ok {
			return fmt.Errorf("sqlite path looks like libpq keyword DSN (%q); use postgres:// for Postgres", param)
		}
	}
	// scheme is "sqlite" or absent — reject libpq-only query params.
	if param, ok := firstPGOnlyQueryParam(dsn); ok {
		label := "sqlite DSN"
		if !hasScheme {
			label = "sqlite path"
		}
		return fmt.Errorf("%s does not support %q query param; did you mean postgres://?", label, param)
	}
	return nil
}

// pgOnlyQueryParams enumerates libpq / pgx query-parameter names that have no
// SQLite analogue. A bare or sqlite:// DSN carrying any of these is almost
// certainly a misformatted postgres DSN.
var pgOnlyQueryParams = []string{
	"sslmode=",
	"pool_max_conns=",
	"application_name=",
	"connect_timeout=",
	"target_session_attrs=",
	"password=",
	"sslpassword=",
}

// firstPGOnlyQueryParam reports whether dsn carries any pg-only query param
// after the first "?". The search is case-sensitive — libpq treats parameter
// names as case-sensitive in URL form.
func firstPGOnlyQueryParam(dsn string) (string, bool) {
	q := strings.Index(dsn, "?")
	if q < 0 {
		return "", false
	}
	query := dsn[q+1:]
	for _, p := range pgOnlyQueryParams {
		if strings.Contains(query, p) {
			// Strip the trailing "=" so the error names the parameter cleanly.
			return strings.TrimSuffix(p, "="), true
		}
	}
	return "", false
}
