package sqlitestore_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type capturedCronSnapshotQuery struct {
	store *sqlitestore.Store
	query string
	args  []any
}

func (q *capturedCronSnapshotQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.store.QueryContext(ctx, query, args...)
}

func (q *capturedCronSnapshotQuery) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.query = query
	q.args = append([]any(nil), args...)
	return q.store.QueryRowContext(ctx, query, args...)
}

// Historical snapshot lookup must use the existing partial cron-event index,
// so it does not search unrelated project events after a definition is compacted.
func TestCronJobSnapshotPayloadUsesCronEventIndex(t *testing.T) {
	store := openTestDB(t)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	capture := &capturedCronSnapshotQuery{store: store}
	_, err = (db.CronSQL{Query: capture}).JobSnapshotPayload(t.Context(), project.ID, "JOB-UID", "DEFINITION-UID")
	require.ErrorIs(t, err, db.ErrNotFound)

	rows, err := store.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+capture.query, capture.args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, strings.Join(plan, "\n"), "idx_events_project_cron", "plan: %v", plan)
}
