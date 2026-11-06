package sqlitestore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"modernc.org/sqlite"
)

type searchQueryConnector struct {
	path    string
	queries int
}

func (c *searchQueryConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *searchQueryConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	return &searchQueryConn{Conn: conn, capture: c}, nil
}

type searchQueryConn struct {
	driver.Conn
	capture *searchQueryConnector
}

func (c *searchQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.capture.queries++
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestSearchFTS_SingleWordQueryCount(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	p := createProject(ctx, t, d, "example-project")
	issue := createTesterIssueWithBody(ctx, t, d, p.ID, "common", "")
	_, _, err := d.CreateComment(ctx, db.CreateCommentParams{IssueID: issue.ID, Author: "tester", Body: "common"})
	require.NoError(t, err)
	capture := &searchQueryConnector{path: path}
	raw := sql.OpenDB(capture)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := &sqlitestore.Store{DB: raw}
	for _, query := range []string{"common", "Common common", "absent"} {
		t.Run(query, func(t *testing.T) {
			capture.queries = 0
			hits, err := store.SearchFTS(ctx, db.SearchFTSParams{ProjectID: p.ID, Query: query})
			require.NoError(t, err)
			require.Equal(t, 1, capture.queries, "single-word search needs only the results query")
			if query == "absent" {
				require.Empty(t, hits)
				return
			}
			require.Len(t, hits, 1)
			require.Equal(t, issue.ID, hits[0].Issue.ID)
			require.InDelta(t, 0.000001, hits[0].Score, 1e-12)
			require.Equal(t, []string{"title", "comments"}, hits[0].MatchedIn)
		})
	}
}
