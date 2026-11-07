package pgstore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestProjectStatsPlanUsesProjectEventIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres testcontainer")
	}
	store, _ := openConformanceStore(t)
	t.Cleanup(func() { _ = store.Close() })
	tx, err := store.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)
	rows, err := tx.QueryContext(t.Context(), "EXPLAIN (COSTS OFF) "+pgstore.BatchProjectStatsSQLForTest, db.SystemProjectName)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var detail string
		require.NoError(t, rows.Scan(&detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "Backward using idx_events_project", "plan: %s", joined)
	require.NotContains(t, joined, "Backward using events_pkey", "plan: %s", joined)
}
