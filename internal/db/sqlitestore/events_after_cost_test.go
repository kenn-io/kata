package sqlitestore_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
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
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = raw.ExecContext(ctx, "ANALYZE")
			require.NoError(t, err)
		}
		for _, tc := range []struct {
			name  string
			after int64
			want  []int64
		}{
			{"from start", 0, []int64{created.ID, move.EventID}},
			{"near tip", move.EventID - 1, []int64{move.EventID}},
		} {
			t.Run(fmt.Sprintf("analyzed=%t/%s", analyzed, tc.name), func(t *testing.T) {
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

// Project polling must bound page work even when many issues left the project.
func TestEventsAfter_DepartureBacklogPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	source := createProject(ctx, t, d, "quiet-project")
	target := createProject(ctx, t, d, "busy-project")
	_, err := d.ExecContext(ctx, `DELETE FROM events`)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO events(id,uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT x,printf('%026d',x),?,?,'busy-project','issue.moved','tester',json_object('from_project_uid',?,'from_short_id',printf('%04x',10001-x)),1,0,? FROM n`, d.InstanceUID(), target.ID, source.UID, strings.Repeat("a", 64))
	require.NoError(t, err)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	raw.SetMaxOpenConns(1)
	store := &sqlitestore.Store{DB: raw}
	for _, analyzed := range []bool{false, true} {
		if analyzed {
			_, err = raw.ExecContext(ctx, "ANALYZE")
			require.NoError(t, err)
		}
		for _, after := range []int64{0, 5000, 9999, 10000} {
			t.Run(fmt.Sprintf("analyzed=%t/after=%d", analyzed, after), func(t *testing.T) {
				conn, err := raw.Conn(ctx)
				require.NoError(t, err)
				sqlitePageFetches(t, conn, true)
				require.NoError(t, conn.Close())
				events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: after, Limit: 100})
				require.NoError(t, err)
				require.NotNil(t, events)
				require.Len(t, events, int(min(100, 10000-after)))
				for i, event := range events {
					require.Equal(t, after+int64(i)+1, event.ID)
				}
				conn, err = raw.Conn(ctx)
				require.NoError(t, err)
				pages := sqlitePageFetches(t, conn, false)
				require.NoError(t, conn.Close())
				t.Logf("10k departures: %d page fetches", pages)
				require.LessOrEqual(t, pages, 512, "project polling must skip earlier departures")
			})
		}
		t.Run(fmt.Sprintf("analyzed=%t/replay", analyzed), func(t *testing.T) {
			var after int64
			for after < 10000 {
				events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: after, Limit: 100})
				require.NoError(t, err)
				require.Len(t, events, 100)
				for _, event := range events {
					require.Equal(t, after+1, event.ID)
					after = event.ID
				}
			}
		})
	}
}

func TestEventsAfter_CombinedProjectFilters(t *testing.T) {
	d, _ := openTestDBWithPath(t)
	ctx := t.Context()
	source := createProject(ctx, t, d, "source-project")
	target := createProject(ctx, t, d, "target-project")
	issue, created := createTesterIssue(ctx, t, d, source.ID, "Moving issue")
	move, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
	require.NoError(t, err)
	other, _ := createTesterIssue(ctx, t, d, source.ID, "Other issue")
	_, err = d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: other.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: other.Revision, Actor: "tester"})
	require.NoError(t, err)
	for _, projectID := range []int64{source.ID, target.ID} {
		events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projectID, IssueUID: issue.UID, Types: []string{"issue.moved"}, ThroughID: move.EventID, Limit: 100})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.Equal(t, move.EventID, events[0].ID)
		events, err = d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projectID, IssueUID: issue.UID, Types: []string{"issue.moved"}, ThroughID: created.ID, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events)
	}
}
