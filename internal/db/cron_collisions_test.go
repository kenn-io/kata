package db

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// The collision read's lookahead prefix is bounded in bytes before Scan,
// including metadata containing multibyte Unicode. This is test-local DDL.
func TestCronCollisionParentMetadataByteReadBound(t *testing.T) {
	pool, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	_, err = pool.ExecContext(t.Context(), `CREATE TABLE issues(id INTEGER, uid TEXT, project_id INTEGER, status TEXT, owner TEXT, author TEXT, metadata TEXT, revision INTEGER, deleted_at TEXT)`)
	require.NoError(t, err)
	metadata := `{"blob":"` + strings.Repeat("😀", 128) + `"}`
	_, err = pool.ExecContext(t.Context(), `INSERT INTO issues VALUES(1,'parent',1,'open','','operator',$1,1,NULL)`, metadata)
	require.NoError(t, err)
	tx, err := pool.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	parent, _, err := (CronSQL{}).recurringParentRead(t.Context(), tx, 1, "parent", false, 128)
	require.NoError(t, err)
	require.LessOrEqual(t, len(parent.Metadata), 128)
}
