package db

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectParentRemovalsBatchesHistoryLookup(t *testing.T) {
	for _, tc := range []struct{ count, queries int }{{3, 1}, {501, 2}} {
		t.Run(fmt.Sprint(tc.count), func(t *testing.T) {
			testDirectParentRemovalsBatch(t, tc.count, tc.queries)
		})
	}
}

func testDirectParentRemovalsBatch(t *testing.T, count, queries int) {
	t.Helper()
	work := &referenceWork{}
	pool := sql.OpenDB(referenceConnector{work})
	t.Cleanup(func() { _ = pool.Close() })
	for _, query := range []string{
		`CREATE TABLE projects(id INTEGER PRIMARY KEY, uid TEXT)`,
		`CREATE TABLE events(uid TEXT, origin_instance_uid TEXT, project_id INTEGER, issue_uid TEXT, related_issue_uid TEXT, type TEXT, actor TEXT, payload TEXT, hlc_physical_ms INTEGER, hlc_counter INTEGER)`,
		`CREATE TABLE links(from_issue_uid TEXT, to_issue_uid TEXT, type TEXT)`,
		`INSERT INTO projects VALUES (1, 'project')`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	var events []RemoteEvent
	for i := range count {
		child := fmt.Sprintf("child-%d", i)
		oldParent := fmt.Sprintf("old-parent-%d", i)
		payload := fmt.Sprintf(`{"type":"parent","from_uid":%q,"to_uid":%q}`, child, oldParent)
		_, err := pool.ExecContext(t.Context(), `INSERT INTO events VALUES ($1,'origin',1,'other',NULL,'issue.linked','actor',$2,1,0)`, fmt.Sprintf("old-%d", i), payload)
		require.NoError(t, err)
		events = append(events, RemoteEvent{EventUID: fmt.Sprintf("new-%d", i), IssueUID: &child, Type: "issue.links_changed", HLCPhysicalMS: 2, Payload: jsontext.Value(fmt.Sprintf(`{"parent_set_uid":"new-parent-%d"}`, i))})
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	*work = referenceWork{}
	removed, err := DirectParentRemovals(t.Context(), tx, events)
	require.NoError(t, err)
	for i := range count {
		require.Equal(t, []string{fmt.Sprintf("old-parent-%d", i)}, removed[fmt.Sprintf("new-%d", i)])
	}
	require.Equal(t, queries, work.queries, "history scans should grow by batch, not by issue")
}
