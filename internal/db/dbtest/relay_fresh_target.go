package dbtest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// RunRelayFreshTargetPolicyEpoch exercises relay fresh target policy epoch on the supplied native store.
// Fresh import recognizes the untouched policy epoch, but refuses later policy
// history or unknown metadata before clearing any target state.
func RunRelayFreshTargetPolicyEpoch(t *testing.T, store db.Storage) {
	ctx := t.Context()
	instance := store.InstanceUID()
	options := db.ImportOptions{RequireFreshTarget: true}
	require.NoError(t, store.ImportReplay(ctx, nil, options), "fresh bootstrap epoch is part of the canonical empty store")
	require.Equal(t, instance, store.InstanceUID())
	sqlStore, ok := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	require.True(t, ok)
	_, err := sqlStore.ExecContext(ctx, `UPDATE meta SET value='2' WHERE key='project_access_revision'`)
	require.NoError(t, err)
	require.Error(t, store.ImportReplay(ctx, nil, options))
	revision, err := store.ProjectAccessRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), revision)
	_, err = sqlStore.ExecContext(ctx, `UPDATE meta SET value='1' WHERE key='project_access_revision'`)
	require.NoError(t, err)
	_, err = sqlStore.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('unknown_local_state','retained')`)
	require.NoError(t, err)
	require.Error(t, store.ImportReplay(ctx, nil, options))
	require.Equal(t, instance, store.InstanceUID())
}
