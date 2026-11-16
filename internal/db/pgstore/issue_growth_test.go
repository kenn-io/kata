package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
	"go.kenn.io/kit/growthtest"
)

// postgresBlockFetches measures an operation by the table and index blocks
// PostgreSQL reads for it instead of elapsed time, which varies with machine
// load. stores returns every store the operation may use. Each must hold one
// connection, because a session's counts reach the statistics views only
// when that session reports them.
func postgresBlockFetches(t *testing.T, stores func() []*pgstore.Store) growthtest.Measure {
	fetched := func() float64 {
		var total float64
		for _, store := range stores() {
			_, err := store.ExecContext(t.Context(), `SELECT pg_stat_force_next_flush()`)
			require.NoError(t, err)
			var blocks float64
			require.NoError(t, store.QueryRowContext(t.Context(), `SELECT COALESCE(sum(heap_blks_read + heap_blks_hit + COALESCE(idx_blks_read, 0) + COALESCE(idx_blks_hit, 0)), 0) FROM pg_statio_user_tables`).Scan(&blocks))
			total += blocks
		}
		return total
	}
	return func(run, reset func()) float64 {
		sample := func() float64 {
			if reset != nil {
				reset()
			}
			before := fetched()
			run()
			return fetched() - before
		}
		// The first run also loads the session's plans.
		sample()
		fewest := sample()
		for range 2 {
			fewest = min(fewest, sample())
		}
		return fewest
	}
}

func TestCreateIssueHistoryGrowth(t *testing.T) {
	var current *pgstore.Store
	dbtest.RunCreateIssueHistoryGrowth(t, func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		store.SetMaxOpenConns(1)
		store.SetMaxIdleConns(1)
		t.Cleanup(func() { _ = store.Close() })
		current = store
		return store
	}, func(t *testing.T, storage db.Storage, project db.Project, historyIssue db.Issue, size int) {
		store := storage.(*pgstore.Store)
		_, err := store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT '01M4'||lpad(x::text,22,'0'),$1,$2,$3,$4,$5,'issue.updated','agent',
CASE x%2 WHEN 1 THEN '{"old_title":"History a","title":"History b"}' ELSE '{"old_title":"History b","title":"History a"}' END,
x,0,lpad(to_hex(x),64,'0') FROM generate_series(1,$6::int) AS n(x)`, store.InstanceUID(), project.ID, project.Name, historyIssue.ID, historyIssue.UID, size)
		require.NoError(t, err)
		_, err = store.ExecContext(t.Context(), "ANALYZE events")
		require.NoError(t, err)
	}, postgresBlockFetches(t, func() []*pgstore.Store { return []*pgstore.Store{current} }), 1)
}
