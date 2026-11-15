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
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: quiet.ID, Limit: 100})
			require.NoError(t, err)
			ids := make([]int64, 0, len(events))
			for _, event := range events {
				ids = append(ids, event.ID)
			}
			require.Equal(t, []int64{quietCreatedID, created.ID, move.EventID}, ids)
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			pages := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("100k unrelated events: %d page fetches", pages)
			require.LessOrEqual(t, pages, 64, "project polling must skip unrelated history")
		})
	}
}

// Project polling must bound page work even when many issues left the project.
func TestEventsAfter_DepartureBacklogPageBudget(t *testing.T) {
	d, path := openTestDBWithPath(t)
	ctx := t.Context()
	source := createProject(ctx, t, d, "quiet-project")
	target := createProject(ctx, t, d, "busy-project")
	_, err := d.ExecContext(ctx, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<10000)
 INSERT INTO issues(uid,project_id,short_id,title,author)
 SELECT printf('%026d',10001-x),?,substr(printf('%026d',10001-x),-4),'Moved issue','tester' FROM n`, target.ID)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT printf('%026d',id+10000),?,?,'quiet-project',id,uid,'issue.created','tester',json_object('title',title),1,0,? FROM issues WHERE project_id=? ORDER BY id`, d.InstanceUID(), source.ID, strings.Repeat("a", 64), target.ID)
	require.NoError(t, err)
	base, err := d.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = d.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT uid,?,?,'busy-project',id,uid,'issue.moved','tester',json_object('issue_uid',uid,'from_project_uid',?,'from_short_id',short_id,'to_project_uid',?,'to_short_id',short_id,'updated_at',updated_at),1,0,? FROM issues WHERE project_id=? ORDER BY id`, d.InstanceUID(), target.ID, source.UID, target.UID, strings.Repeat("a", 64), target.ID)
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
		t.Run(fmt.Sprintf("analyzed=%t", analyzed), func(t *testing.T) {
			conn, err := raw.Conn(ctx)
			require.NoError(t, err)
			sqlitePageFetches(t, conn, true)
			require.NoError(t, conn.Close())
			events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: source.ID, AfterID: base, Limit: 100})
			require.NoError(t, err)
			require.NotNil(t, events)
			require.Len(t, events, 100)
			for i, event := range events {
				require.Equal(t, base+int64(i)+1, event.ID)
			}
			conn, err = raw.Conn(ctx)
			require.NoError(t, err)
			pages := sqlitePageFetches(t, conn, false)
			require.NoError(t, conn.Close())
			t.Logf("10k departures: %d page fetches", pages)
			require.LessOrEqual(t, pages, 512, "project polling must bound departure page work")
		})
	}
}

func TestEventsAfter_CombinedProjectFilters(t *testing.T) {
	d := openTestDB(t)
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
		t.Run(fmt.Sprintf("project=%d", projectID), func(t *testing.T) {
			eventType, expectedID := "issue.moved", move.EventID
			if projectID == source.ID {
				eventType, expectedID = "issue.created", created.ID
			}
			events, err := d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projectID, IssueUID: issue.UID, Types: []string{eventType}, ThroughID: move.EventID, Limit: 100})
			require.NoError(t, err)
			require.Len(t, events, 1)
			require.Equal(t, expectedID, events[0].ID)
			events, err = d.EventsAfter(ctx, db.EventsAfterParams{ProjectID: projectID, IssueUID: issue.UID, Types: []string{"issue.moved"}, ThroughID: created.ID, Limit: 100})
			require.NoError(t, err)
			require.Empty(t, events)
		})
	}
	t.Run("restricted", func(t *testing.T) {
		spoke := createProject(ctx, t, d, "spoke-project")
		events, err := d.EventsAfter(db.WithAuthorizedProjects(ctx, []string{spoke.UID}), db.EventsAfterParams{ProjectID: source.ID, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, events)
	})
}
