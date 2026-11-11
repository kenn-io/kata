package sqlitestore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestTrackerAttemptLookupUsesMetaPrimaryKeyRange(t *testing.T) {
	store := openTestDB(t)
	base, err := db.TrackerExecutionFenceKey("01ARZ3NDEKTSV4RRFFQ69G5FAV", "01ARZ3NDEKTSV4RRFFQ69G5FAW", "executor")
	require.NoError(t, err)
	prefix := base + ".attempt." + strings.Repeat("a", 64) + "."
	rows, err := store.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+db.TrackerAttemptLookupSQL, prefix+"00000000000000000001", prefix+"09223372036854775808")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "SEARCH meta")
	require.NotContains(t, joined, "SCAN meta")
}
