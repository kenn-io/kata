package pgstore

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePostgresTransportRejectsUnverifiedRemoteCandidates(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{
		"postgres://user:secret@db.example/kata",
		"postgres://user:secret@db.example/kata?sslmode=disable",
		"postgres://user:secret@db.example/kata?sslmode=allow",
		"postgres://user:secret@db.example/kata?sslmode=prefer",
		"postgres://user:secret@db.example/kata?sslmode=require",
		"postgres://user:secret@db.example/kata?sslmode=verify-ca",
	} {
		t.Run(dsn, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(dsn)
			require.NoError(t, err)
			err = validatePostgresTransport(cfg, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "verified TLS")
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestValidatePostgresTransportAcceptsVerifiedRemoteAndLocalPlaintext(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{
		"postgres://user@db.example/kata?sslmode=verify-full&sslrootcert=system",
		"postgres://user@db.example/kata?sslmode=verify-ca&sslrootcert=system",
		"postgres://user@127.0.0.1/kata?sslmode=disable",
		"postgres://user@[::1]/kata?sslmode=disable",
		"postgres://user@localhost/kata?sslmode=disable",
		"postgres://user@/kata?host=/var/run/postgresql&sslmode=disable",
	} {
		t.Run(dsn, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(dsn)
			require.NoError(t, err)
			assert.NoError(t, validatePostgresTransport(cfg, false))
		})
	}
}

func TestValidatePostgresTransportExplicitInsecureOptIn(t *testing.T) {
	t.Parallel()
	cfg, err := pgx.ParseConfig("postgres://user@db.example/kata?sslmode=disable")
	require.NoError(t, err)
	assert.NoError(t, validatePostgresTransport(cfg, true))
}
