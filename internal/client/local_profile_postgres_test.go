package client

import (
	"database/sql"
	"fmt"
	"testing"

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
