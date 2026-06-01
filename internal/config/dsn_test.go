package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
)

func TestCanonicalDSNIdentityBarePathUnchanged(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("/home/u/.kata/kata.db")
	require.NoError(t, err)
	assert.Equal(t, "/home/u/.kata/kata.db", got)
}

func TestCanonicalDSNIdentityPostgresStripsCredentialsAndParams(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@db.example.com:5432/kata?sslmode=require")
	require.NoError(t, err)
	assert.Equal(t, "postgres://db.example.com/kata", got)
	assert.NotContains(t, got, "SECRET")
}

func TestCanonicalDSNIdentityPostgresStripsDefaultPort(t *testing.T) {
	// 5432 is the postgres default port — strip it so the same logical DB
	// referenced with or without :5432 produces the same identity.
	withPort, err := config.CanonicalDSNIdentity("postgres://host:5432/kata")
	require.NoError(t, err)
	noPort, err := config.CanonicalDSNIdentity("postgres://host/kata")
	require.NoError(t, err)
	assert.Equal(t, noPort, withPort)
	assert.Equal(t, "postgres://host/kata", withPort)

	// A non-default port is preserved.
	custom, err := config.CanonicalDSNIdentity("postgres://host:6543/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://host:6543/kata", custom)
}

func TestCanonicalDSNIdentityPostgresNoPortOmitsColon(t *testing.T) {
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@db.example.com/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://db.example.com/kata", got)
}

func TestCanonicalDSNIdentityUnknownSchemeErrors(t *testing.T) {
	_, err := config.CanonicalDSNIdentity("mysql://h/db")
	require.Error(t, err)
}

func TestRedactDSNRemovesPassword(t *testing.T) {
	got := config.RedactDSN("postgres://user:SECRET@db.example.com:5432/kata?sslmode=require")
	assert.NotContains(t, got, "SECRET")
	assert.Contains(t, got, "user")
	assert.Contains(t, got, "db.example.com")
	// Mutation guard: the raw DSN really does contain the secret, so the
	// NotContains assertion above is non-vacuous.
	assert.Contains(t, "postgres://user:SECRET@db.example.com:5432/kata?sslmode=require", "SECRET")
}

func TestRedactDSNBarePathUnchanged(t *testing.T) {
	assert.Equal(t, "/home/u/.kata/kata.db", config.RedactDSN("/home/u/.kata/kata.db"))
}

func TestRedactDSNStripsCredentialsInQueryString(t *testing.T) {
	// Postgres URLs can carry credentials in the query (libpq accepts
	// ?password=...&sslpassword=...) — RedactDSN drops the whole query for
	// display so the leak surface stays bounded regardless of which key
	// carries the secret.
	dsn := "postgres://db.example.com/kata?password=SECRET&sslmode=require" //nolint:gosec // fixture proves the query-string credential is dropped
	got := config.RedactDSN(dsn)
	assert.NotContains(t, got, "SECRET")
	assert.NotContains(t, got, "password=")
	assert.NotContains(t, got, "?")
	// Mutation guard: the raw DSN really does contain the secret.
	assert.Contains(t, dsn, "SECRET")
}

func TestCanonicalDSNIdentityRejectsAmbiguousCredentials(t *testing.T) {
	// A password containing unencoded "://" confuses url.Parse (u.User comes
	// back nil, the credential ends up in u.Host/u.Path). Defensive: refuse to
	// canonicalize rather than emit an identity that embeds the credential.
	_, err := config.CanonicalDSNIdentity("postgres://user:p://w@host/db")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "p://w")
	// Mutation guard: the raw input really does contain the credential chars.
	assert.Contains(t, "postgres://user:p://w@host/db", "p://w")
}

func TestRedactDSNRejectsAmbiguousCredentials(t *testing.T) {
	// Same defensive behavior for redaction: an unencoded "://" in the password
	// makes url.Parse populate User=nil; without a defense, the input would be
	// echoed unchanged. Returning "" is safe.
	got := config.RedactDSN("postgres://user:p://w@host/db")
	assert.Equal(t, "", got)
	// Mutation guard.
	assert.Contains(t, "postgres://user:p://w@host/db", "p://w")
}

func TestCanonicalDSNIdentityErrorOmitsCredentials(t *testing.T) {
	// url.Parse returns a *url.Error whose Error() includes the raw input;
	// wrapping with %w would leak the password through error logs.
	_, err := config.CanonicalDSNIdentity("postgres://user:SECRET@host:badport/db")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SECRET")
	// Mutation guard.
	assert.Contains(t, "postgres://user:SECRET@host:badport/db", "SECRET")
}

func TestCanonicalDSNIdentityPreservesAtInDatabasePath(t *testing.T) {
	// A legitimate "@" in the database-path segment (e.g. pgbouncer-style
	// dbname@tenant) yields u.Path == "/db@tenant", which does NOT match the
	// credential-bleed shape (the bleed produces a path starting with "//"
	// because the misparsed "://" leaves a residual slash). Must canonicalize
	// normally.
	got, err := config.CanonicalDSNIdentity("postgres://host/db@tenant")
	require.NoError(t, err)
	assert.Equal(t, "postgres://host/db@tenant", got)
}

func TestRedactDSNPreservesAtInDatabasePath(t *testing.T) {
	// Same as above for redaction: a path-segment "@" must round-trip cleanly.
	got := config.RedactDSN("postgres://host/db@tenant")
	assert.Equal(t, "postgres://host/db@tenant", got)
}

func TestCanonicalDSNIdentityBracketsIPv6Host(t *testing.T) {
	// IPv6 hosts must be bracketed in the canonical form so the result is a
	// valid URL and two semantically distinct DSNs cannot collide.
	got, err := config.CanonicalDSNIdentity("postgres://user:SECRET@[::1]:5432/kata")
	require.NoError(t, err)
	// Default port (5432) is stripped; brackets stay.
	assert.Equal(t, "postgres://[::1]/kata", got)
	assert.NotContains(t, got, "SECRET")

	got2, err := config.CanonicalDSNIdentity("postgres://[2001:db8::1]:6543/kata")
	require.NoError(t, err)
	assert.Equal(t, "postgres://[2001:db8::1]:6543/kata", got2)

	// Mutation guard: confirm the input had the secret + a non-default port.
	assert.Contains(t, "postgres://user:SECRET@[::1]:5432/kata", "SECRET")
}
