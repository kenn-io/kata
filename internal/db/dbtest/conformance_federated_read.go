package dbtest

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kit/search/sqlquery"
)

// checkFederatedReadPages puts unrelated history wider than any page window
// before each visibility source, so every source is reached through a skip.
func checkFederatedReadPages(t *testing.T, store db.Storage, backend Backend) {
	ctx := t.Context()
	quiet, err := store.CreateProject(ctx, "quiet-project")
	require.NoError(t, err)
	busy, err := store.CreateProject(ctx, "busy-project")
	require.NoError(t, err)
	for _, p := range []db.Project{quiet, busy} {
		_, err = store.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
	}
	incoming, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Incoming", Author: "tester"})
	require.NoError(t, err)
	outgoing, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Outgoing", Author: "tester"})
	require.NoError(t, err)
	returning, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Returning", Author: "tester"})
	require.NoError(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	executor := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	gap := func() {
		_, err := executor.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<200)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT '00000000000000000000'||CAST(100000+(SELECT COALESCE(MAX(id),0) FROM events)+x AS TEXT),$1,$2,'busy-project','project.metadata_updated','tester','{}',1,0,$3 FROM n`, store.InstanceUID(), busy.ID, strings.Repeat("a", 64))
		require.NoError(t, err)
	}

	gap()
	title := "Incoming history"
	incoming, history, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: incoming.ID, Title: &title, Actor: "tester"})
	require.NoError(t, err)
	want := []int64{history.ID}
	for _, field := range []string{"issue_uid", "uid"} {
		gap()
		event := newRemoteEvent(t, busy, nil, "issue.updated", "tester", store.InstanceUID(), time.Now().UnixMilli(), jsontext.Value(`{"`+field+`":"`+incoming.UID+`","title":"Payload history"}`))
		_, err = store.InsertRemoteEvents(ctx, busy.ID, []db.RemoteEvent{event})
		require.NoError(t, err)
		stored, err := store.EventsByUIDs(ctx, busy.ID, []string{event.EventUID})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		want = append(want, stored[0].ID)
	}
	gap()
	created, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: quiet.ID, Actor: "tester", Patch: map[string]jsontext.Value{"purpose": jsontext.Value(`"Local event"`)}})
	require.NoError(t, err)
	gap()
	out, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: outgoing.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: outgoing.Revision, Actor: "tester"})
	require.NoError(t, err)
	in, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: incoming.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: incoming.Revision, Actor: "tester"})
	require.NoError(t, err)
	away, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: returning.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: returning.Revision, Actor: "tester"})
	require.NoError(t, err)
	returning, err = store.IssueByUID(ctx, returning.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	back, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: returning.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: returning.Revision, Actor: "tester"})
	require.NoError(t, err)
	want = append(want, created.Event.ID, out.EventID, in.EventID, away.EventID, back.EventID)

	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok, "federated page checks require transaction access")
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	// Small pages must return what one large page returns, in order.
	for _, limit := range []int{1, 2, 3, 100} {
		ids := []int64{}
		for cursor := after; ; {
			events, err := db.ReadFederationEventPage(ctx, tx, quiet, "", cursor, limit, backend.Name == "postgres", func(where string, args ...any) ([]db.Event, error) {
				return (sqlquery.Query{SQL: `SELECT e.id FROM events e` + where, Args: args}).All(ctx, tx, func(rows *sql.Rows) (db.Event, error) { //nolint:gosec // G202: helper-built SQL; values stay bound.
					var event db.Event
					return event, rows.Scan(&event.ID)
				})
			})
			require.NoError(t, err)
			for _, event := range events {
				ids = append(ids, event.ID)
				cursor = event.ID
			}
			if len(events) < limit {
				break
			}
		}
		require.Equal(t, want, ids, "limit %d", limit)
	}
}

// SeedFederatedReadBulkHistory puts long source histories behind the cursor,
// then moves every subject into the destination through the public store API.
func SeedFederatedReadBulkHistory(t *testing.T, store db.Storage, interleaved bool) (db.Project, int64) {
	t.Helper()
	ctx := t.Context()
	source, err := store.CreateProject(ctx, "source-project")
	require.NoError(t, err)
	target, err := store.CreateProject(ctx, "target-project")
	require.NoError(t, err)
	other, err := store.CreateProject(ctx, "other-project")
	require.NoError(t, err)
	for _, p := range []db.Project{source, target} {
		_, err = store.EnableProjectFederation(ctx, p.ID, "tester")
		require.NoError(t, err)
	}
	issues := make([]db.Issue, 200)
	for i := range issues {
		issues[i], _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "Incoming", Author: "tester"})
		require.NoError(t, err)
	}
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	stride := 1
	if interleaved {
		stride = 2
	}
	executor := store.(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	_, err = executor.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100*$4)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,issue_id,issue_uid,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT '00000000000000000000'||CAST(100000+$5+(i.id-$6)*100*$4+x AS TEXT),$1,CASE WHEN x%$4=0 THEN CAST($2 AS BIGINT) ELSE CAST($3 AS BIGINT) END,CASE WHEN x%$4=0 THEN 'source-project' ELSE 'other-project' END,
 CASE WHEN x%$4=0 THEN i.id ELSE NULL END,CASE WHEN x%$4=0 THEN i.uid ELSE NULL END,'issue.updated','tester','{}',1,0,$7
 FROM issues i CROSS JOIN n WHERE i.project_id=$2 ORDER BY i.id,x`, store.InstanceUID(), source.ID, other.ID, stride, after, issues[0].ID, strings.Repeat("a", 64))
	require.NoError(t, err)
	for _, issue := range issues {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	return target, after
}
