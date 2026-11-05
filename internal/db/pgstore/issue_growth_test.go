package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestCreateIssueHistoryGrowth(t *testing.T) {
	dbtest.RunCreateIssueHistoryGrowth(t, func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		t.Cleanup(func() { _ = store.Close() })
		return store
	}, func(t *testing.T, storage db.Storage, project db.Project, historyIssue db.Issue, size int) {
		store := storage.(*pgstore.Store)
		_, err := store.ExecContext(t.Context(), `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
SELECT '01M4'||lpad(x::text,22,'0'),$1,$2,$3,$4,$5,'issue.updated','agent',
CASE x%2 WHEN 1 THEN '{"old_title":"History a","title":"History b"}' ELSE '{"old_title":"History b","title":"History a"}' END,
x,0,lpad(to_hex(x),64,'0') FROM generate_series(1,$6::int) AS n(x)`, store.InstanceUID(), project.ID, project.Name, historyIssue.ID, historyIssue.UID, size)
		require.NoError(t, err)
		_, err = store.ExecContext(t.Context(), "ANALYZE events")
		require.NoError(t, err)
	})
}
