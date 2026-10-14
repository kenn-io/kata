package sqlitestore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The acknowledgement check reads only the interval above the settled cursor.
// SQLite must reach it through the pending-delivery index with the id range,
// not by scanning the binding's retained history.
func TestRelayAckUnemittedCountUsesPendingRange(t *testing.T) {
	store, err := Open(t.Context(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rows, err := store.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+relayAckUnemittedCountSQL, "binding", "events", 1, 10, 20)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	joined := strings.Join(plan, "\n")
	require.Contains(t, joined, "USING INDEX idx_relay_outbox_pending", "plan: %s", joined)
	require.Contains(t, joined, "id>? AND id<?", "plan: %s", joined)
}
