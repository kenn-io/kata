package sqlitestore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func TestProjectStatsPlanUsesProjectIndexes(t *testing.T) {
	store := openUIStore(t)
	rows, err := store.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+batchProjectStatsSQL, db.SystemProjectName)
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
	require.Contains(t, joined, "idx_issues_project_status_updated", "plan: %s", joined)
	require.Contains(t, joined, "COVERING INDEX idx_events_project (project_id=?)", "plan: %s", joined)
	for _, detail := range plan {
		fields := strings.Fields(detail)
		if len(fields) < 2 || fields[0] != "SCAN" {
			continue
		}
		require.NotContains(t, []string{"issues", "events", "i", "e", "m"}, fields[1], "plan: %s", joined)
	}
}
