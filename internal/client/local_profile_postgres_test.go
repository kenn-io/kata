package client

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
)

func TestLocalProfilePostgresStorageInspectionRetainsUnsupportedSchema(t *testing.T) {
	dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
	t.Cleanup(cleanup)
	database, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	const uid = "01HZZZZZZZZZZZZZZZZZZZZZ01"
	// This minimal test-local metadata fixture deliberately has no product
	// projections. Unsupported schemas must be diagnosed before opening them.
	_, err = database.ExecContext(t.Context(), `CREATE SCHEMA profile_metadata;
		CREATE TABLE profile_metadata.meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO profile_metadata.meta VALUES ('instance_uid', '01HZZZZZZZZZZZZZZZZZZZZZ01'), ('schema_version', '1')`)
	require.NoError(t, err)
	profile := config.LocalProfileConfig{DSN: dsn, InstanceUID: uid, Config: &config.DaemonConfig{}}
	profile.Config.Storage.Postgres.Schema = "profile_metadata"
	for _, version := range []int{db.CurrentSchemaVersion() - 1, db.CurrentSchemaVersion() + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			_, err := database.ExecContext(t.Context(), `UPDATE profile_metadata.meta SET value=$1 WHERE key='schema_version'`, fmt.Sprint(version))
			require.NoError(t, err)
			identity, err := InspectLocalProfileStorage(t.Context(), profile)
			require.NoError(t, err)
			require.Equal(t, uid, identity.InstanceUID)
			require.Equal(t, version, identity.SchemaVersion)
			_, err = EnsureLocalProfileSelection(t.Context(), DaemonSelection{Profile: &profile, Resolved: ResolvedDaemon{LocalProfile: &LocalProfileIdentity{InstanceUID: uid}}})
			require.ErrorIs(t, err, ErrProfileSchemaMismatch)
			var stored string
			require.NoError(t, database.QueryRowContext(t.Context(), `SELECT value FROM profile_metadata.meta WHERE key='schema_version'`).Scan(&stored))
			require.Equal(t, fmt.Sprint(version), stored, "inspection and refused recovery cannot change metadata")
		})
	}
}

// PostgreSQL in validate mode performs no DDL, so a replacement daemon starts
// only on the binary's exact schema. The default-home auto-start must leave
// the live daemon running rather than stop it for a daemon that cannot start.
func TestEnsureRunningKeepsDaemonWhenPostgresValidateModeCannotOpenSchema(t *testing.T) {
	older := db.CurrentSchemaVersion() - 1
	for _, tt := range []struct {
		name     string
		mode     string
		schema   int
		replaced bool
	}{
		{name: "validate older schema", mode: "validate", schema: older, replaced: false},
		{name: "validate current schema", mode: "validate", schema: db.CurrentSchemaVersion(), replaced: true},
		{name: "bootstrap older schema", mode: "bootstrap", schema: older, replaced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dsn, cleanup := testenv.NewPostgresContainer(t, t.Context())
			t.Cleanup(cleanup)
			database, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = database.Close() })
			_, err = database.ExecContext(t.Context(), `CREATE SCHEMA ensure_meta;
				CREATE TABLE ensure_meta.meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
			require.NoError(t, err)
			_, err = database.ExecContext(t.Context(),
				`INSERT INTO ensure_meta.meta VALUES ('schema_version', $1)`, fmt.Sprint(tt.schema))
			require.NoError(t, err)

			t.Setenv("KATA_SKIP_DAEMON_VERSION_CHECK", "")
			tmp := setupKataEnv(t)
			t.Setenv("KATA_DSN", dsn)
			t.Setenv("KATA_POSTGRES_SCHEMA", "ensure_meta")
			t.Setenv("KATA_POSTGRES_SCHEMA_MODE", tt.mode)
			_, addr := startMockDaemonPing(t, map[string]any{
				"ok": true, "service": "kata", "version": "old-build", "pid": os.Getpid(),
			})
			require.NoError(t, writeRuntimeRecord(t, tmp, addr))
			state := patchEnsureHooks(t, "new-build", "http://replacement-daemon")

			url, err := EnsureRunning(t.Context())

			if tt.replaced {
				require.NoError(t, err)
				assert.Equal(t, "http://replacement-daemon", url)
				assert.Equal(t, 1, state.stopCalls)
				return
			}
			require.ErrorIs(t, err, ErrDaemonNotReplaceable)
			assert.Zero(t, state.stopCalls, "the live daemon must not be stopped")
			assert.Zero(t, state.startCalls)
			for _, want := range []string{
				"old-build", "new-build",
				fmt.Sprintf("schema_version %d", tt.schema),
				fmt.Sprintf("binary schema %d", db.CurrentSchemaVersion()),
				"kata storage postgres migrate",
			} {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}
