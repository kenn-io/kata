package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestListPageHashPreservesCallerFilters(t *testing.T) {
	store, err := sqlitestore.Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	params := db.ListAllIssuesParams{AllowedProjectIDs: []int64{2, 1, 2}}
	hash := listPageHash(ServerConfig{DB: store}, "global-list", params)
	require.Equal(t, []int64{2, 1, 2}, params.AllowedProjectIDs)
	equivalent := db.ListAllIssuesParams{AllowedProjectIDs: []int64{1, 2}}
	require.Equal(t, hash, listPageHash(ServerConfig{DB: store}, "global-list", equivalent))
}
