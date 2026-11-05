package sqlitestore_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"modernc.org/sqlite"
)

// Interleave real writes immediately before the event page reaches SQLite.
type eventAdoptionConnector struct {
	path       string
	beforePage func()
	query      string
	args       []any
}

func (c *eventAdoptionConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *eventAdoptionConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	return &eventAdoptionConn{Conn: conn, capture: c}, nil
}

type eventAdoptionConn struct {
	driver.Conn
	capture *eventAdoptionConnector
}

func (c *eventAdoptionConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FROM events e") && c.capture.beforePage != nil {
		before := c.capture.beforePage
		c.capture.beforePage = nil
		before()
		c.capture.query = query
		for _, arg := range args {
			c.capture.args = append(c.capture.args, arg.Value)
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestEventsAfter_ProjectUIDAdoptionDuringCursorRead(t *testing.T) {
	for _, analyzed := range []bool{false, true} {
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
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
			if analyzed {
				_, err = d.ExecContext(ctx, "ANALYZE")
				require.NoError(t, err)
			}
			var moveID, laterID int64
			capture := &eventAdoptionConnector{path: path, beforePage: func() {
				// The old implementation has already read source.UID here.
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
				_, later := createTesterIssue(ctx, t, d, source.ID, "Later issue")
				laterID = later.ID
			}}
			raw := sql.OpenDB(capture)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			store := &sqlitestore.Store{DB: raw}
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: created.ID, Limit: 100})
			require.NoError(t, err)
			require.Nil(t, capture.beforePage, "adoption must interleave with the poll")
			require.NotEmpty(t, events)
			var ids []int64
			for _, event := range events {
				ids = append(ids, event.ID)
			}
			if events[len(events)-1].ID >= moveID {
				require.Contains(t, ids, moveID, "a page must not advance past an omitted departure")
			}
			if events[len(events)-1].ID >= laterID {
				reset, err := d.PurgeResetCheck(ctx, laterID, source.ID)
				require.NoError(t, err)
				require.Zero(t, reset, "a later reset cannot recover a skipped departure")
			}
			rows, err := d.QueryContext(ctx, "EXPLAIN QUERY PLAN "+capture.query, capture.args...)
			require.NoError(t, err)
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			joined := strings.Join(plan, "\n")
			t.Log(joined)
			require.Contains(t, joined, "SEARCH e USING INDEX idx_events_moved_from_id")
		})
	}
}
