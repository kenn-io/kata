package jsonl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

// These history fixtures exercise a legacy export path. Reconstruct its
// physical mapping shape as well as its version, rather than mislabel v31 DDL.
func setSchema30Fixture(ctx context.Context, t *testing.T, source *sqlitestore.Store) {
	t.Helper()
	var legacy int
	require.NoError(t, source.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('import_mappings') WHERE name='status_sync_json'`).Scan(&legacy))
	if legacy == 0 {
		var populated int
		require.NoError(t, source.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_mappings WHERE observed_status IS NOT NULL OR observed_status_at IS NOT NULL OR pending_event_uid IS NOT NULL OR remote_locator IS NOT NULL`).Scan(&populated))
		require.Zero(t, populated, "history-only fixtures must not discard a private checkpoint")
		for _, column := range []string{"observed_status", "observed_status_at", "pending_event_uid", "remote_locator"} {
			_, err := source.ExecContext(ctx, `ALTER TABLE import_mappings DROP COLUMN `+column)
			require.NoError(t, err)
		}
		_, err := source.ExecContext(ctx, `ALTER TABLE import_mappings ADD COLUMN status_sync_json TEXT`)
		require.NoError(t, err)
	}
	_, err := source.ExecContext(ctx, `UPDATE meta SET value='30' WHERE key='schema_version'`)
	require.NoError(t, err)
}
