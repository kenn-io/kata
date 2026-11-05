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

func TestEventsAfter_QuietProjectPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	busy := createProject(ctx, t, d, "busy-project")
	quiet := createProject(ctx, t, d, "quiet-project")
	spoke := createProject(ctx, t, d, "spoke-project")
	// Project catalog events aren't part of this issue-history fixture.
	_, err := d.ExecContext(ctx, `DELETE FROM events`)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'busy-project','issue.created','tester','{}',1,0,? FROM n`, d.InstanceUID(), busy.ID, strings.Repeat("a", 64))
	require.NoError(t, err)
	issue, created := createTesterIssue(ctx, t, d, quiet.ID, "Moving issue")
	move, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	store := &sqlitestore.Store{DB: raw}
	for _, tc := range []struct {
		name  string
		after int64
		want  []int64
	}{
		{"from start", 0, []int64{created.ID, move.EventID}},
		{"near tip", move.EventID - 1, []int64{move.EventID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: quiet.ID, AfterID: tc.after, Limit: 100})
			require.NoError(t, err)
			ids := make([]int64, 0, len(events))
			for _, event := range events {
				ids = append(ids, event.ID)
			}
			require.Equal(t, tc.want, ids)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			pages := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("100k unrelated events: %d page fetches", pages)
			require.LessOrEqual(t, pages, 256, "project polling must skip unrelated history")
		})
	}
	t.Run("destination includes move once", func(t *testing.T) {
		events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: busy.ID, AfterID: created.ID, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, move.EventID, events[0].ID)
	})
	t.Run("restricted", func(t *testing.T) {
		events, err := store.EventsAfter(db.WithAuthorizedProjects(ctx, []string{spoke.UID}), db.EventsAfterParams{ProjectID: quiet.ID, Limit: 100})
		require.NoError(t, err)
		require.NotNil(t, events)
		require.Empty(t, events)
	})
}

// Capture the statement executed by EventsAfter so the plan assertion follows
// the production query, including its bound project UID.
type eventPlanConnector struct {
	path  string
	query string
	args  []any
}

func (c *eventPlanConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *eventPlanConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	return &eventPlanConn{Conn: conn, capture: c}, nil
}

type eventPlanConn struct {
	driver.Conn
	capture *eventPlanConnector
}

func (c *eventPlanConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FROM events e") {
		c.capture.query = query
		c.capture.args = nil
		for _, arg := range args {
			c.capture.args = append(c.capture.args, arg.Value)
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func TestEventsAfterPlanUsesProjectIndexes(t *testing.T) {
	d, path := openTestDBWithPath(t)
	project := createProject(t.Context(), t, d, "quiet-project")
	busy := createProject(t.Context(), t, d, "busy-project")
	_, err := d.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<1000)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'busy-project','issue.created','tester','{}',1,0,? FROM n`, d.InstanceUID(), busy.ID, strings.Repeat("a", 64))
	require.NoError(t, err)
	issue, _ := createTesterIssue(t.Context(), t, d, project.ID, "Moving issue")
	_, err = d.MoveIssueProject(t.Context(), db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: project.ID, ToProjectID: busy.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	capture := &eventPlanConnector{path: path}
	raw := sql.OpenDB(capture)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := &sqlitestore.Store{DB: raw}
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = d.ExecContext(t.Context(), "ANALYZE")
			require.NoError(t, err)
		}
		_, err = store.EventsAfter(t.Context(), db.EventsAfterParams{ProjectID: project.ID, Limit: 100})
		require.NoError(t, err)
		require.NotEmpty(t, capture.query)
		rows, err := d.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+capture.query, capture.args...)
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
		require.Contains(t, joined, "idx_events_project", "analyzed=%t: %s", analyzed, joined)
		require.Contains(t, joined, "idx_events_moved_from", "analyzed=%t: %s", analyzed, joined)
		for _, detail := range plan {
			fields := strings.Fields(detail)
			if len(fields) >= 2 && fields[0] == "SCAN" {
				require.NotEqual(t, "e", fields[1], "analyzed=%t: %s", analyzed, joined)
			}
		}
	}
}
