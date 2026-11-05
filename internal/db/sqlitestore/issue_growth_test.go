package sqlitestore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

func TestCreateIssueHistoryGrowth(t *testing.T) {
	dbtest.RunCreateIssueHistoryGrowth(t, func(t *testing.T) db.Storage {
		return openTestDB(t)
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
	})
}
