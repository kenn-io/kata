package sqlitestore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"modernc.org/sqlite"
)

type commentUIDLookupCaptureConnector struct {
	driver.Connector
	capture func(string, []driver.NamedValue)
}

func (c commentUIDLookupCaptureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return commentUIDLookupCaptureConn{Conn: conn, capture: c.capture}, nil
}

func (c commentUIDLookupCaptureConnector) Driver() driver.Driver {
	return c.Connector.Driver()
}

type commentUIDLookupCaptureConn struct {
	driver.Conn
	capture func(string, []driver.NamedValue)
}

func (c commentUIDLookupCaptureConn) QueryContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	c.capture(query, args)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestCommentIssueIDsByUIDsUsesHandleIndex(t *testing.T) {
	ctx := t.Context()
	base, err := sqlite.NewConnector(":memory:")
	require.NoError(t, err)
	var capturedQuery string
	var capturedArgs []any
	pool := sql.OpenDB(commentUIDLookupCaptureConnector{
		Connector: base,
		capture: func(query string, args []driver.NamedValue) {
			capturedQuery = query
			capturedArgs = make([]any, len(args))
			for i, arg := range args {
				capturedArgs[i] = arg.Value
			}
		},
	})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	for _, statement := range []string{
		`CREATE TABLE comments(uid TEXT NOT NULL, issue_id INTEGER NOT NULL)`,
		`CREATE INDEX idx_comments_uid_handle ON comments(lower(substr(uid,21)))`,
		`INSERT INTO comments(uid,issue_id) VALUES('01M40000000000000000000001',1)`,
	} {
		_, err := pool.ExecContext(ctx, statement)
		require.NoError(t, err)
	}

	store := &sqlitestore.Store{DB: pool}
	got, err := store.CommentIssueIDsByUIDs(ctx, []string{"01m40000000000000000000001"})
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"01m40000000000000000000001": 1}, got)
	require.NotEmpty(t, capturedQuery, "the production lookup must issue a query")

	rows, err := pool.QueryContext(ctx, "EXPLAIN QUERY PLAN "+capturedQuery, capturedArgs...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, strings.Join(plan, "\n"), "idx_comments_uid_handle",
		"the actual canonical comment UID lookup must use the existing suffix index")
}
