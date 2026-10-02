package storeopen_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/db/storeopen"
	"go.kenn.io/kata/internal/version"
)

// Contract vmr7: development binaries need explicit consent before upgrading
// existing SQLite state, and refusal must identify the home without writes.
func TestDevelopmentMigrationAdmission(t *testing.T) {
	for _, build := range []string{"dev", "(devel)", "g1234567", "v0.18.0-dirty", "v0.18.0-3-g1234567", "v0.18.0"} {
		for _, optIn := range []bool{false, true} {
			t.Run(build+map[bool]string{false: "/refuse", true: "/opt-in"}[optIn], func(t *testing.T) {
				original := version.Version
				version.Version = build
				t.Cleanup(func() { version.Version = original })
				home := t.TempDir()
				t.Setenv("KATA_HOME", home)
				t.Setenv("KATA_ALLOW_DEV_MIGRATION", map[bool]string{false: "", true: "1"}[optIn])
				path := filepath.Join(home, "kata.db")
				stageLegacyPreCutoverFixture(t, path, db.CurrentSchemaVersion()-1)
				before, err := os.ReadFile(path) //nolint:gosec // G304: path is a temporary database created by this test.
				require.NoError(t, err)
				s, err := storeopen.Open(t.Context(), path)
				if optIn || build == "v0.18.0" {
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
				require.ErrorContains(t, err, "development build")
				require.ErrorContains(t, err, home)
				require.ErrorContains(t, err, "KATA_ALLOW_DEV_MIGRATION=1")
				after, err := os.ReadFile(path) //nolint:gosec // G304: path is a temporary database created by this test.
				require.NoError(t, err)
				require.Equal(t, before, after)
				backups, err := filepath.Glob(path + ".bak.*")
				require.NoError(t, err)
				require.Empty(t, backups)
			})
		}
	}
}
