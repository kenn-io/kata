package pgstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePostgresTransportRejectsUnverifiedRemoteCandidates(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGSERVICEFILE", "")
	t.Setenv("PGSSLMODE", "")
	t.Setenv("PGSSLROOTCERT", "")

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

func TestValidatePostgresTransportAcceptsEffectiveVerifiedTLS(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		t.Setenv("PGSERVICE", "")
		t.Setenv("PGSERVICEFILE", "")
		t.Setenv("PGSSLMODE", "verify-full")
		t.Setenv("PGSSLROOTCERT", "system")
		cfg, err := pgx.ParseConfig("postgres://user@db.example/kata")
		require.NoError(t, err)
		assert.NoError(t, validatePostgresTransport(cfg, false))
	})

	t.Run("service file", func(t *testing.T) {
		serviceFile := filepath.Join(t.TempDir(), "pg_service.conf")
		require.NoError(t, os.WriteFile(serviceFile, []byte(`[verified]
host=db.example
port=5432
dbname=kata
sslmode=verify-full
sslrootcert=system
`), 0o600))
		t.Setenv("PGSERVICEFILE", serviceFile)
		t.Setenv("PGSERVICE", "")
		t.Setenv("PGSSLMODE", "")
		t.Setenv("PGSSLROOTCERT", "")
		cfg, err := pgx.ParseConfig("postgres://user@/?service=verified")
		require.NoError(t, err)
		assert.NoError(t, validatePostgresTransport(cfg, false))
	})
}
