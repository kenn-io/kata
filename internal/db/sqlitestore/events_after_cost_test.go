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
	quietCreatedID, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	spoke := createProject(ctx, t, d, "spoke-project")
	busyIssue, _ := createTesterIssue(ctx, t, d, busy.ID, "Busy issue")
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'busy-project',?,?,'issue.updated','tester',json_object('title',printf('Update %d',x)),1,0,? FROM n`, d.InstanceUID(), busy.ID, busyIssue.ID, busyIssue.UID, strings.Repeat("a", 64))
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
			{"from start", 0, []int64{quietCreatedID, created.ID, move.EventID}},
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
	issue, _ := createTesterIssue(ctx, t, d, target.ID, "Moved issue")
	base, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',x),?,?,'busy-project',?,?,'issue.moved','tester',json_object('issue_uid',?,'from_project_uid',?,'from_short_id',printf('%026d',10001-x),'to_project_uid',?,'to_short_id',?,'updated_at',?),1,0,? FROM n`, d.InstanceUID(), target.ID, issue.ID, issue.UID, issue.UID, source.UID, target.UID, issue.ShortID, issue.UpdatedAt, strings.Repeat("a", 64))
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
				events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: base + after, Limit: 100})
				require.NoError(t, err)
				require.NotNil(t, events)
				require.Len(t, events, int(min(100, 10000-after)))
				for i, event := range events {
					require.Equal(t, base+after+int64(i)+1, event.ID)
				}
				conn, err = raw.Conn(ctx)
				require.NoError(t, err)
				pages := sqlitePageFetches(t, conn, false)
				require.NoError(t, conn.Close())
				t.Logf("10k departures: %d page fetches", pages)
				require.LessOrEqual(t, pages, 512, "project polling must skip earlier departures")
			})
		}
	}
}

func TestEventsAfter_CombinedProjectFilters(t *testing.T) {
	d, _ := openTestDBWithPath(t)
	ctx := t.Context()
	source := createProject(ctx, t, d, "source-project")
	target := createProject(ctx, t, d, "target-project")
	// Keep the other issue's events inside both ThroughID bounds.
	other, _ := createTesterIssue(ctx, t, d, source.ID, "Other issue")
	_, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: other.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: other.Revision, Actor: "tester"})
	require.NoError(t, err)
	issue, created := createTesterIssue(ctx, t, d, source.ID, "Moving issue")
	move, err := d.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
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
