package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// checkFederatedQuietPollFeed follows one spoke's cursor through polls of a
// quiet project while another store handle on the same database, as another
// process would, writes every kind of event that can reach the project's
// feed. An empty poll moves the cursor to the newest event the hub had, and
// polls from that cursor must still return every later feed event. History
// filed elsewhere before an issue moved in sits below the cursor; the spoke
// reads it through the issue's own history, as it does after any move.
//
// The quiet project runs once with one issue and once with more issues than
// most gaps below have events, because the store checks a short gap event by
// event and a long one against the project's issues.
func checkFederatedQuietPollFeed(t *testing.T, store db.Storage, backend Backend) {
	for _, extraIssues := range []int{0, 20} {
		t.Run(fmt.Sprintf("%d extra issues", extraIssues), func(t *testing.T) {
			checkFederatedQuietPollFeedWith(t, store, backend, extraIssues)
		})
	}
}

func checkFederatedQuietPollFeedWith(t *testing.T, store db.Storage, backend Backend, extraIssues int) {
	ctx := t.Context()
	writer := backend.OpenSame(t, store)
	quiet, err := writer.CreateProject(ctx, fmt.Sprintf("quiet-project-%d", extraIssues))
	require.NoError(t, err)
	busy, err := writer.CreateProject(ctx, fmt.Sprintf("busy-project-%d", extraIssues))
	require.NoError(t, err)
	unbound, err := writer.CreateProject(ctx, fmt.Sprintf("unbound-project-%d", extraIssues))
	require.NoError(t, err)
	newest, err := writer.MaxEventID(ctx)
	require.NoError(t, err)
	for _, after := range []int64{0, newest} {
		_, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: unbound.ID, Features: db.SupportedEventFeatures, AfterID: after, Limit: 1000})
		require.ErrorIs(t, err, db.ErrNotFound)
	}
	for _, p := range []db.Project{quiet, busy} {
		_, err = writer.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
	}
	owned, _, err := writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Owned", Author: "tester"})
	require.NoError(t, err)
	for range extraIssues {
		_, _, err = writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Extra", Author: "tester"})
		require.NoError(t, err)
	}
	incoming, _, err := writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Incoming", Author: "tester"})
	require.NoError(t, err)
	busyIssue, _, err := writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Busy", Author: "tester"})
	require.NoError(t, err)
	executor := writer.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = executor.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES($1,$2)`, fmt.Sprintf("unrelated-text-%d", extraIssues), "not a cursor")
	require.NoError(t, err)
	var cursor int64
	// poll reads one page from the cursor, moves the cursor where the hub
	// says, and requires the page to hold want.
	poll := func(want ...int64) db.FederationReadResult {
		t.Helper()
		out, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 1000})
		require.NoError(t, err)
		got := make([]int64, 0, len(out.Events))
		for _, event := range out.Events {
			got = append(got, event.ID)
		}
		for _, id := range want {
			require.Contains(t, got, id, "poll after %d", cursor)
		}
		require.GreaterOrEqual(t, out.NextAfterID, cursor)
		cursor = out.NextAfterID
		return out
	}
	// quiet writes past the cursor in another project and requires the next
	// poll to return nothing and move the cursor past those writes.
	quietPoll := func() {
		t.Helper()
		_, _, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: busyIssue.ID, Body: "busy", Author: "tester"})
		require.NoError(t, err)
		newest, err := writer.MaxEventID(ctx)
		require.NoError(t, err)
		require.Empty(t, poll().Events)
		require.Equal(t, newest, cursor, "an empty poll moves the cursor to the newest event")
	}
	for drained := false; !drained; {
		drained = len(poll().Events) == 0
	}
	quietPoll()

	// An event stamped to another project about an issue the project owns,
	// arriving after the hub wrote more than one page window past the cursor.
	appendUnrelatedEvents(t, writer, busy, busyIssue, extraIssues*10000, 1100)
	foreignUID := fmt.Sprintf("01M8%022d", extraIssues)
	_, err = executor.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($5,$1,$2,$6,$3,'issue.updated','tester','{}',1,0,$4)`, writer.InstanceUID(), busy.ID, owned.UID, strings.Repeat("a", 64), foreignUID, busy.Name)
	require.NoError(t, err)
	foreign, err := writer.EventsByUIDs(ctx, busy.ID, []string{foreignUID})
	require.NoError(t, err)
	require.Len(t, foreign, 1)
	poll(foreign[0].ID)

	// History filed elsewhere before an issue moves in falls below the cursor
	// once an empty poll passes it. The move reaches the feed, and the issue's
	// own history still holds the earlier comment.
	_, history, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: incoming.ID, Body: "before the move", Author: "tester"})
	require.NoError(t, err)
	quietPoll()
	require.Less(t, history.ID, cursor)
	incoming, err = writer.IssueByUID(ctx, incoming.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	moveIn, err := writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: incoming.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: incoming.Revision, Actor: "tester"})
	require.NoError(t, err)
	poll(moveIn.EventID)
	issueHistory, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, IssueUID: incoming.UID, Limit: 1000})
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(issueHistory.Events, func(e db.Event) bool { return e.ID == history.ID }), "the moved issue's history holds the comment filed before the move")

	// A move out of the project is stamped to the destination.
	quietPoll()
	owned, err = writer.IssueByUID(ctx, owned.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	moveOut, err := writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: owned.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: owned.Revision, Actor: "tester"})
	require.NoError(t, err)
	poll(moveOut.EventID)

	// The project's own events.
	quietPoll()
	_, own, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: incoming.ID, Body: "after the move", Author: "tester"})
	require.NoError(t, err)
	poll(own.ID)

	// A moved-in issue's cron delivery state makes the feed require the cron
	// feature. Only a hub cron dispatch writes that state, so the fixture sets
	// it directly.
	quietPoll()
	require.NotContains(t, poll().RequiredFeatures, db.CronEventFeature)
	moveInDelivered := func() db.Issue {
		t.Helper()
		delivered, _, err := writer.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Delivered", Author: "tester"})
		require.NoError(t, err)
		_, err = executor.ExecContext(ctx, `UPDATE issues SET metadata=$1 WHERE id=$2`, `{"cron.progress.example":true}`, delivered.ID)
		require.NoError(t, err)
		moved, err := writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: delivered.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: delivered.Revision, Actor: "tester"})
		require.NoError(t, err)
		require.Contains(t, poll(moved.EventID).RequiredFeatures, db.CronEventFeature)
		quietPoll()
		_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.IssueMoveEventFeature, AfterID: cursor, Limit: 1000})
		require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
		delivered, err = writer.IssueByUID(ctx, delivered.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		return delivered
	}

	// Moving that issue out drops the requirement.
	delivered := moveInDelivered()
	_, err = writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: delivered.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: delivered.Revision, Actor: "tester"})
	require.NoError(t, err)
	require.NotContains(t, poll().RequiredFeatures, db.CronEventFeature)

	// So does purging it.
	delivered = moveInDelivered()
	_, err = writer.PurgeIssue(ctx, delivered.ID, "tester", nil)
	require.NoError(t, err)
	out, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: cursor, Limit: 1000})
	require.NoError(t, err)
	require.NotContains(t, out.RequiredFeatures, db.CronEventFeature)
	require.Greater(t, out.ResetAfterID, cursor)
}
