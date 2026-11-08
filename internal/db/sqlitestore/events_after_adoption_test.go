package sqlitestore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"modernc.org/sqlite"
)

// Interleave real writes immediately before a matching read reaches SQLite.
type beforeQueryConnector struct {
	path        string
	contains    string
	beforeQuery func()
}

func (c *beforeQueryConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *beforeQueryConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	return &beforeQueryConn{Conn: conn, capture: c}, nil
}

type beforeQueryConn struct {
	driver.Conn
	capture *beforeQueryConnector
}

func (c *beforeQueryConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *beforeQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, c.capture.contains) && c.capture.beforeQuery != nil {
		before := c.capture.beforeQuery
		c.capture.beforeQuery = nil
		before()
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestEventsAfter_ProjectUIDAdoptionDuringCursorRead(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	source := createProject(ctx, t, d, "source-project")
	target := createProject(ctx, t, d, "target-project")
	issue, created := createTesterIssue(ctx, t, d, source.ID, "Moving issue")
	_, err := d.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID: target.ID, HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: newTestUID(t), Actor: "tester",
	})
	require.NoError(t, err)
	var moveID int64
	capture := &beforeQueryConnector{path: path, contains: "FROM events e", beforeQuery: func() {
		// A separate UID pre-read would retain source.UID across adoption here.
		_, err := d.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
			ProjectID: source.ID, HubURL: "https://hub.example", HubProjectID: 41,
			HubProjectUID: newTestUID(t), Actor: "tester",
		})
		require.NoError(t, err)
		move, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{
			IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID,
			IfMatchRev: issue.Revision, Actor: "tester",
		})
		require.NoError(t, err)
		moveID = move.EventID
	}}
	raw := sql.OpenDB(capture)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := &sqlitestore.Store{DB: raw}
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: created.ID, Limit: 100})
	require.NoError(t, err)
	require.Nil(t, capture.beforeQuery, "adoption must interleave with the poll")
	var ids []int64
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	require.Contains(t, ids, moveID, "a page must include the departure after adoption")
}
