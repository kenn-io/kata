package storeopen_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/version"
)

func TestEmbeddedServiceUpgradesWithoutCLIConsent(t *testing.T) {
	original := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = original })
	t.Setenv("KATA_ALLOW_DEV_MIGRATION", "")
	path := filepath.Join(t.TempDir(), "kata.db")
	stageLegacyPreCutoverFixture(t, path, db.CurrentSchemaVersion()-1)
	service, err := kata.New(t.Context(), kata.Config{
		DSN: path, Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	require.NoError(t, err)
	require.NoError(t, service.Close())
	ver, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, db.CurrentSchemaVersion(), ver)
}

// Storage policy is explicit: neither the host build nor CLI environment
// variables may override the caller's choice.
func TestSQLiteMigrationAdmissionUsesExplicitConfig(t *testing.T) {
	original := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = original })
	for _, blocked := range []bool{false, true} {
		for _, env := range []string{"", "1"} {
			t.Run(fmt.Sprintf("blocked=%t/env=%s", blocked, env), func(t *testing.T) {
				t.Setenv("KATA_ALLOW_DEV_MIGRATION", env)
				path := filepath.Join(t.TempDir(), "kata.db")
				stageLegacyPreCutoverFixture(t, path, db.CurrentSchemaVersion()-1)
				before, err := os.ReadFile(path) //nolint:gosec // G304: temporary test database.
				require.NoError(t, err)
				s, err := storeopen.OpenWithConfig(t.Context(), path, storeopen.Config{RequireMigrationConsent: blocked})
				if !blocked {
					require.NoError(t, err)
					require.NoError(t, s.Close())
					ver, err := sqlitestore.PeekSchemaVersion(t.Context(), path)
					require.NoError(t, err)
					require.Equal(t, db.CurrentSchemaVersion(), ver)
					return
				}
				if s != nil {
					_ = s.Close()
				}
				require.ErrorIs(t, err, storeopen.ErrMigrationConsentRequired)
				after, err := os.ReadFile(path) //nolint:gosec // G304: temporary test database.
				require.NoError(t, err)
				require.Equal(t, before, after)
				backups, err := filepath.Glob(path + ".bak.*")
				require.NoError(t, err)
				require.Empty(t, backups)
				// Refusing an upgrade must also release the database lock.
				s, err = storeopen.Open(t.Context(), path)
				require.NoError(t, err)
				require.NoError(t, s.Close())
			})
		}
	}
}
