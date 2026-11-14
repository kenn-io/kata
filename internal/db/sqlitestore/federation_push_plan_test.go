package sqlitestore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestFederationPendingPushUsesCursorRange(t *testing.T) {
	store := openTestDB(t)
	query := `SELECT e.id FROM events e WHERE e.project_id=$1 AND e.origin_instance_uid=$2 AND e.id IN (` + db.FederationPushPendingIDs("$1", "$2", "$3") + `)`
	rows, err := store.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, 1, store.InstanceUID(), 100)
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
	require.Contains(t, strings.Join(plan, "\n"), "id>?", "pending events must seek past the cursor")
}
