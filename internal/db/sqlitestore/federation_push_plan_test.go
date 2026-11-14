package sqlitestore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestFederationPendingPushUsesCursorRange(t *testing.T) {
	store := openTestDB(t)
	project, err := store.CreateProject(t.Context(), "push-budget")
	require.NoError(t, err)
	for range 100 {
		_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: project.ID, Title: "pending work", Author: "tester"})
		require.NoError(t, err)
	}
	cursor, err := store.MaxEventID(t.Context())
	require.NoError(t, err)
	pending, err := store.PendingFederationPushEvents(t.Context(), project.ID, store.InstanceUID(), cursor, 100)
	require.NoError(t, err)
	require.Empty(t, pending)
	query := sqlitestore.EventSelectForTest + sqlitestore.PendingFederationPushWhereForTest
	rows, err := store.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, project.ID, store.InstanceUID(), cursor, 100)
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
