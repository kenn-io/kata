package dbtest

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kata/internal/db"
)

func checkScreenViewClaims(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			claimed, err := store.ClaimScreenView(ctx, "issues", "2026-10-02")
			if err != nil {
				t.Error(err)
				return
			}
			if claimed {
				claims.Add(1)
			}
		})
	}
	wg.Wait()
	require.Equal(t, int32(1), claims.Load())
	claimed, err := store.ClaimScreenView(ctx, "issues", "2026-10-03")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.ReleaseScreenView(ctx, "issues", "2026-10-02"))
	claimed, err = store.ClaimScreenView(ctx, "issues", "2026-10-03")
	require.NoError(t, err)
	require.False(t, claimed, "old rollback preserves the new day")
	require.NoError(t, store.ReleaseScreenView(ctx, "issues", "2026-10-03"))
	claimed, err = store.ClaimScreenView(ctx, "issues", "2026-10-03")
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = store.ClaimScreenView(ctx, "help", "2026-10-03")
	require.NoError(t, err)
	require.True(t, claimed)
	var recs []db.ImportRecord
	var count int
	for row, err := range store.ExportMeta(ctx) {
		require.NoError(t, err)
		recs = append(recs, &db.MetaKV{Key: row.Key, Value: row.Value})
		if strings.HasPrefix(row.Key, "screen_viewed:") {
			count++
		}
	}
	require.Equal(t, 2, count, "later dates replace a bounded screen key")
	for _, rec := range recs {
		row := rec.(*db.MetaKV)
		if row.Key == "instance_uid" {
			row.Value = "01HZNQ7VFPK1XGD8R5MABCD4EX"
		}
	}
	require.NoError(t, store.ImportReplay(ctx, recs, db.ImportOptions{}))
	claimed, err = store.ClaimScreenView(ctx, "issues", "2026-10-03")
	require.NoError(t, err)
	require.True(t, claimed, "a changed installation starts a fresh count")
	return nil
}
