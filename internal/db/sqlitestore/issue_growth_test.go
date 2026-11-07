package sqlitestore_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestCreateIssueHistoryGrowth(t *testing.T) {
	var current *sql.DB
	pages := func(reset bool) int {
		conn, err := current.Conn(t.Context())
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		return sqlitePageFetches(t, conn, reset)
	}
	dbtest.RunCreateIssueHistoryGrowth(t, func(t *testing.T) db.Storage {
		store, path := openTestDBWithPath(t)
		// The write-queue wrapper doesn't expose the driver's page counters.
		raw, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, raw.Close()) })
		raw.SetMaxOpenConns(1)
		current = raw
		measured := *store
		measured.DB = raw
		return &measured
	}, func(t *testing.T, storage db.Storage, project db.Project, historyIssue db.Issue, size int) {
		store := storage.(*sqlitestore.Store)
		_, err := store.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (
    VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<?
)
INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT printf('01M4%022d',x),?,?,?,?,?,'issue.updated','agent',
CASE x%2 WHEN 1 THEN '{"old_title":"History a","title":"History b"}' ELSE '{"old_title":"History b","title":"History a"}' END,
x,0,printf('%064x',x) FROM n`, size, store.InstanceUID(), project.ID, project.Name, historyIssue.ID, historyIssue.UID)
		require.NoError(t, err)
	}, func(run, reset func()) float64 {
		// Count database work instead of elapsed time, which varies with CI load.
		reset()
		pages(true)
		run()
		return float64(pages(false))
	}, 1)
}
