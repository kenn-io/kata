package sqlitestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"modernc.org/sqlite"
)

type moveDecisionConnector struct {
	path  string
	reads int
}

func (c *moveDecisionConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *moveDecisionConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	return &moveDecisionConn{Conn: conn, capture: c}, nil
}

type moveDecisionConn struct {
	driver.Conn
	capture *moveDecisionConnector
}

func (c *moveDecisionConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *moveDecisionConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT id,deleted_at IS NULL FROM projects WHERE uid=") {
		c.capture.reads++
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestMoveRebuildReusesFirstDecisionAndRefreshesLater(t *testing.T) {
	ctx := t.Context()
	d, source, issue := incrementalTestStore(t)
	destination, err := d.CreateProject(ctx, "destination")
	require.NoError(t, err)
	_, err = d.EnableProjectFederation(ctx, destination.ID, "tester")
	require.NoError(t, err)
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `UPDATE issues SET project_id=?,short_id=? WHERE id=?`, source.ID, issue.ShortID, issue.ID)
	require.NoError(t, err)
	prepared, err := d.prepareFederationRebuild(ctx, source.ID)
	require.NoError(t, err)
	require.Len(t, prepared.projects, 2)
	require.Len(t, prepared.projects[0].decisions, 1)
	require.Len(t, prepared.projects[1].decisions, 1)
	capture := &moveDecisionConnector{path: d.Path()}
	raw := sql.OpenDB(capture)
	raw.SetMaxOpenConns(1)
	original := d.DB
	d.DB = raw
	defer func() { d.DB = original; require.NoError(t, raw.Close()) }()
	require.NoError(t, d.applyFederationRebuild(ctx, prepared))
	require.Equal(t, 3, capture.reads, "validate both projects and refresh only the later rehome")
	got, err := d.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, destination.ID, got.ProjectID)
}
