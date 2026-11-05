package pgstore_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/dbtest"
	"go.kenn.io/kata/internal/db/pgstore"
)

func TestIssueUIDPrefixGrowth(t *testing.T) {
	dbtest.RunIssueUIDPrefixGrowth(t, func(t *testing.T) db.Storage {
		store, _ := openConformanceStore(t)
		t.Cleanup(func() { _ = store.Close() })
		return store
	}, func(t *testing.T, store db.Storage, size, offset int, prefix string, evenProject, oddProject int64) {
		postgres := store.(*pgstore.Store)
		_, err := postgres.ExecContext(t.Context(), `INSERT INTO issues(uid,short_id,project_id,title,author)
SELECT $2||lpad(x::text,22,'0'),lpad(x::text,6,'0'),CASE x%2 WHEN 0 THEN $3::bigint ELSE $4::bigint END,'Unrelated issue','agent' FROM generate_series(1+$5::integer,$1+$5::integer) n(x)`, size, prefix, evenProject, oddProject, offset)
		require.NoError(t, err)
		_, err = postgres.ExecContext(t.Context(), `ANALYZE issues`)
		require.NoError(t, err)
	})
}
