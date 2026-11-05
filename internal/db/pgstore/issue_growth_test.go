package pgstore_test

import (
	"strings"
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
SELECT '01M4'||lpad(x::text,22,'0'),$1,$2,$3,$6,$7,'issue.updated','agent','{}',x,0,$4
FROM generate_series(1,$5::int) AS n(x)`, store.InstanceUID(), project.ID, project.Name, strings.Repeat("a", 64), size, historyIssue.ID, historyIssue.UID)
		require.NoError(t, err)
		_, err = store.ExecContext(t.Context(), "ANALYZE events")
		require.NoError(t, err)
	})
}
