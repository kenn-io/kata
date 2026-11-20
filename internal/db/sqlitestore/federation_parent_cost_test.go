package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

type parentQueryConnector struct {
	queuedConnector
	queries atomic.Int64
}

func (c *parentQueryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.queuedConnector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &parentQueryConn{sqliteConn: conn.(sqliteConn), queries: &c.queries}, nil
}

type parentQueryConn struct {
	sqliteConn
	queries *atomic.Int64
}

func (c *parentQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if query == "SELECT to_issue_uid FROM links WHERE from_issue_uid=$1 AND type='parent'" {
		c.queries.Add(1)
	}
	return c.sqliteConn.QueryContext(ctx, query, args)
}

func TestFederationIngestOrdinaryParentAssignmentAvoidsTrackerQuery(t *testing.T) {
	d, project, child := incrementalTestStore(t)
	ctx := t.Context()
	parent, _, err := d.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "parent", Author: "tester"})
	require.NoError(t, err)
	drv, err := registeredSQLiteDriver()
	require.NoError(t, err)
	capture := &parentQueryConnector{queuedConnector: queuedConnector{dsn: "file:" + sqliteURIPath(d.Path()) + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", driver: drv, queue: newWriteQueue()}}
	original := d.DB
	d.DB = sql.OpenDB(capture)
	t.Cleanup(func() { require.NoError(t, original.Close()) })
	event := incrementalRemoteEvent(t, project, child.UID, "issue.linked", `{"type":"parent","from_uid":"`+child.UID+`","to_uid":"`+parent.UID+`"}`, 9_000_000_000_000)
	result, err := d.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: event.OriginInstanceUID, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.NoError(t, err)
	require.Equal(t, 1, result.Accepted)
	link, err := d.ParentOf(ctx, child.ID)
	require.NoError(t, err)
	require.Equal(t, parent.UID, link.ToIssueUID)
	require.Zero(t, capture.queries.Load(), "ordinary pushes do not need the parent tracker")
}
