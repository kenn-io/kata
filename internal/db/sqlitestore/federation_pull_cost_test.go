package sqlitestore

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFullPullRowValidationPageBudget(t *testing.T) {
	ctx := t.Context()
	d, p, _ := incrementalTestStore(t)
	_, err := d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
INSERT INTO issues(uid,project_id,short_id,title,author) SELECT printf('%026d',x),?,printf('%010d',x),'issue','tester' FROM n`, p.ID)
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	var pages [2]int
	for i, group := range [][]int64{nil, {p.ID}} {
		conn, err := raw.Conn(ctx)
		require.NoError(t, err)
		cronPullPageFetches(t, conn, true)
		require.NoError(t, conn.Close())
		tx, err := raw.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err)
		_, err = federationRowStamp(ctx, tx, p.ID, group)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		conn, err = raw.Conn(ctx)
		require.NoError(t, err)
		pages[i] = cronPullPageFetches(t, conn, false)
		require.NoError(t, conn.Close())
	}
	require.LessOrEqual(t, pages[1], pages[0]+100, "a mixed pull with no sibling rows must avoid scanning the project twice")
}
