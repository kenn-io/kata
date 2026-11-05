package sqlitestore_test

import (
	"strings"
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
SELECT printf('01M4%022d',x),?,?,?,?,?,'issue.updated','agent','{}',x,0,? FROM n`, size, store.InstanceUID(), project.ID, project.Name, historyIssue.ID, historyIssue.UID, strings.Repeat("a", 64))
		require.NoError(t, err)
	})
}
