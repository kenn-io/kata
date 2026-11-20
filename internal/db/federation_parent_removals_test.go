package db

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func parentRemovalFixture(t *testing.T) (*sql.Tx, *referenceWork) {
	t.Helper()
	work := &referenceWork{}
	pool := sql.OpenDB(referenceConnector{work})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	for _, query := range []string{
		`CREATE TABLE links(from_issue_uid TEXT,to_issue_uid TEXT,type TEXT)`,
		`CREATE TABLE events(uid TEXT,origin_instance_uid TEXT,issue_uid TEXT,related_issue_uid TEXT,type TEXT,payload TEXT,hlc_physical_ms INTEGER,hlc_counter INTEGER)`,
	} {
		_, err := pool.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	tx, err := pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback()) })
	return tx, work
}

func TestParentRemovalsReadsSharedParentHistoryOncePerChild(t *testing.T) {
	tx, work := parentRemovalFixture(t)
	var events []RemoteEvent
	for i := range 100 {
		child := fmt.Sprintf("child-%d", i)
		_, err := tx.ExecContext(t.Context(), `INSERT INTO links VALUES($1,'parent','parent')`, child)
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO events VALUES($1,'origin',$2,'parent','issue.linked',$3,10,0)`, child, child, fmt.Sprintf(`{"type":"parent","from_uid":%q,"to_uid":"parent"}`, child))
		require.NoError(t, err)
		events = append(events, RemoteEvent{EventUID: child, IssueUID: &child, Type: "issue.links_changed", HLCPhysicalMS: 20, Payload: []byte(`{"parent_set_uid":"new-parent"}`)})
	}
	work.rows = 0
	removed, err := DirectParentRemovals(t.Context(), tx, events)
	require.NoError(t, err)
	require.Len(t, removed, 100)
	require.LessOrEqual(t, work.rows, 300, "reading one shared parent must not multiply its link history")
}

func TestParentRemovalsKeepsWinningAssignmentClock(t *testing.T) {
	tx, _ := parentRemovalFixture(t)
	_, err := tx.ExecContext(t.Context(), `INSERT INTO links VALUES('child','parent','parent')`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO events VALUES('snapshot','origin','parent',NULL,'issue.snapshot','{"links":[{"type":"parent","to_issue_uid":"child","incoming":true}]}',20,0)`)
	require.NoError(t, err)
	child := "child"
	events := []RemoteEvent{
		{EventUID: "stale", IssueUID: &child, Type: "issue.links_changed", HLCPhysicalMS: 10, Payload: []byte(`{"parent_set_uid":"old"}`)},
		{EventUID: "new", IssueUID: &child, Type: "issue.links_changed", HLCPhysicalMS: 30, Payload: []byte(`{"parent_set_uid":"new"}`)},
		{EventUID: "newer", IssueUID: &child, Type: "issue.links_changed", HLCPhysicalMS: 40, Payload: []byte(`{"parent_set_uid":"last"}`)},
	}
	removed, err := DirectParentRemovals(t.Context(), tx, events)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"new": {"parent"}, "newer": {"new"}}, removed)
}
