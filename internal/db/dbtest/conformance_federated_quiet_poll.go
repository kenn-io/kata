package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// checkFederatedQuietPollFeed repeats one spoke's polls of a quiet project
// while another store handle on the same database, as another process would,
// writes every kind of event that can reach the project's feed. Each poll must
// return what a store that has never polled returns.
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
	unrelated := func() {
		t.Helper()
		_, _, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: busyIssue.ID, Body: "busy", Author: "tester"})
		require.NoError(t, err)
	}
	ids := func(s db.Storage, after int64) []int64 {
		t.Helper()
		out, err := s.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, Features: db.SupportedEventFeatures, AfterID: after, Limit: 1000})
		require.NoError(t, err)
		got := make([]int64, 0, len(out.Events))
		for _, event := range out.Events {
			got = append(got, event.ID)
		}
		return got
	}
	// poll reads through the long-lived store, checks it against a store
	// that has never polled, and requires polls to have seen want.
	poll := func(after int64, want ...int64) []int64 {
		t.Helper()
		got := ids(store, after)
		require.Equal(t, ids(backend.OpenSame(t, store), after), got, "poll after %d", after)
		for _, id := range want {
			require.Contains(t, got, id, "poll after %d", after)
		}
		return got
	}
	// quietAt repeats an empty poll across unrelated writes so the store
	// remembers what those polls found.
	quietAt := func(after int64) {
		t.Helper()
		for range 3 {
			unrelated()
			require.Empty(t, poll(after))
		}
	}
	var cursor int64
	for page := poll(0); len(page) > 0; page = poll(cursor) {
		cursor = page[len(page)-1]
	}

	// An event stamped to another project about an issue the project owns,
	// arriving after the hub wrote more than one page window past the cursor,
	// where the store checks newer events one at a time.
	appendUnrelatedEvents(t, writer, busy, busyIssue, extraIssues*10000, 1100)
	quietAt(cursor)
	foreignUID := fmt.Sprintf("01M8%022d", extraIssues)
	executor := writer.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = executor.ExecContext(ctx, `INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 VALUES($5,$1,$2,$6,$3,'issue.updated','tester','{}',1,0,$4)`, writer.InstanceUID(), busy.ID, owned.UID, strings.Repeat("a", 64), foreignUID, busy.Name)
	require.NoError(t, err)
	foreign, err := writer.EventsByUIDs(ctx, busy.ID, []string{foreignUID})
	require.NoError(t, err)
	require.Len(t, foreign, 1)
	got := poll(cursor, foreign[0].ID)
	cursor = got[len(got)-1]

	// History recorded elsewhere joins the feed when its issue moves in, even
	// though the store already found that range quiet.
	_, history, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: incoming.ID, Body: "before the move", Author: "tester"})
	require.NoError(t, err)
	quietAt(cursor)
	incoming, err = writer.IssueByUID(ctx, incoming.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	moveIn, err := writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: incoming.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: incoming.Revision, Actor: "tester"})
	require.NoError(t, err)
	got = poll(cursor, history.ID, moveIn.EventID)
	cursor = got[len(got)-1]

	// A move out of the project is stamped to the destination.
	quietAt(cursor)
	owned, err = writer.IssueByUID(ctx, owned.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	moveOut, err := writer.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: owned.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: owned.Revision, Actor: "tester"})
	require.NoError(t, err)
	got = poll(cursor, moveOut.EventID)
	cursor = got[len(got)-1]

	// The project's own events.
	quietAt(cursor)
	_, own, err := writer.CreateComment(ctx, db.CreateCommentParams{IssueID: incoming.ID, Body: "after the move", Author: "tester"})
	require.NoError(t, err)
	poll(cursor, own.ID)
}
