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

func checkFederatedReadPages(t *testing.T, store db.Storage, backend Backend) error {
	ctx := t.Context()
	fixture := SeedFederatedReadLifecycle(t, store, func(_ db.Project, busy db.Project) {
		executor := store.(interface {
			ExecContext(context.Context, string, ...any) (sql.Result, error)
		})
		_, err := executor.ExecContext(ctx, `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<200)
 INSERT INTO events(uid,origin_instance_uid,project_id,project_name,type,actor,payload,hlc_physical_ms,hlc_counter,content_hash)
 SELECT '00000000000000000000'||CAST(100000+(SELECT COALESCE(MAX(id),0) FROM events)+x AS TEXT),$1,$2,'busy-project','project.metadata_updated','tester','{}',1,0,$3 FROM n`, store.InstanceUID(), busy.ID, strings.Repeat("a", 64))
		require.NoError(t, err)
	})
	quiet, after, want := fixture.Project, fixture.AfterID, fixture.EventIDs
	// Small pages must preserve order while skipping gaps before every source.
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	require.True(t, ok, "federated page checks require transaction access")
	tx, err := beginner.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	for _, limit := range []int{1, 2, 3, 100} {
		ids := []int64{}
		for cursor := after; ; {
			events, err := db.ReadFederationEventPage(ctx, tx, quiet, "", cursor, limit, backend.Name == "postgres", func(where string, args ...any) ([]db.Event, error) {
				return QueryFederatedReadEventIDs(ctx, tx, `SELECT e.id FROM events e`+where, args...) //nolint:gosec // G202: helper-built SQL; values stay bound.
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
		require.Equal(t, want, ids)
	}

	events, err := db.ReadFederationEventPage(ctx, tx, quiet, fixture.Incoming.UID, after, 100, backend.Name == "postgres", func(where string, args ...any) ([]db.Event, error) {
		return QueryFederatedReadEventIDs(ctx, tx, `SELECT e.id FROM events e`+where, args...) //nolint:gosec // G202: helper-built SQL; values stay bound.
	})
	require.NoError(t, err)
	ids := []int64{}
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	require.Equal(t, fixture.IncomingEventIDs, ids)
	require.NoError(t, tx.Rollback())
	page, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
	require.NoError(t, err)
	ids = []int64{}
	for _, event := range page.Events {
		ids = append(ids, event.ID)
	}
	require.Equal(t, want, ids)

	_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: quiet.ID, IssueUID: fixture.Outgoing.UID, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
	require.ErrorIs(t, err, db.ErrNotFound)
	return nil
}

// FederatedReadLifecycle covers local, incoming, outgoing, and returning events.
type FederatedReadLifecycle struct {
	Project, Busy              db.Project
	Incoming, Outgoing         db.Issue
	AfterID                    int64
	EventIDs, IncomingEventIDs []int64
}

// SeedFederatedReadLifecycle lets cost tests add unrelated history after the cursor.
func SeedFederatedReadLifecycle(t *testing.T, store db.Storage, beforeHistory func(db.Project, db.Project)) FederatedReadLifecycle {
	t.Helper()
	ctx := t.Context()
	quiet, err := store.CreateProject(ctx, "quiet-project")
	require.NoError(t, err)
	busy, err := store.CreateProject(ctx, "busy-project")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, quiet.ID, "tester")
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, busy.ID, "tester")
	require.NoError(t, err)
	incoming, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: busy.ID, Title: "Incoming", Author: "tester"})
	require.NoError(t, err)
	outgoing, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Outgoing", Author: "tester"})
	require.NoError(t, err)
	returning, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: quiet.ID, Title: "Returning", Author: "tester"})
	require.NoError(t, err)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}
	title := "Incoming history"
	incoming, history, _, err := store.EditIssue(ctx, db.EditIssueParams{IssueID: incoming.ID, Title: &title, Actor: "tester"})
	require.NoError(t, err)
	payloadIDs := []int64{}
	for _, field := range []string{"issue_uid", "uid"} {
		if beforeHistory != nil {
			beforeHistory(quiet, busy)
		}
		event := newRemoteEvent(t, busy, nil, "issue.updated", "tester", store.InstanceUID(), time.Now().UnixMilli(), jsontext.Value(`{"`+field+`":"`+incoming.UID+`","title":"Payload history"}`))
		_, err = store.InsertRemoteEvents(ctx, busy.ID, []db.RemoteEvent{event})
		require.NoError(t, err)
		stored, err := store.EventsByUIDs(ctx, busy.ID, []string{event.EventUID})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		payloadIDs = append(payloadIDs, stored[0].ID)
	}
	unrelated := newRemoteEvent(t, busy, nil, "project.metadata_updated", "tester", store.InstanceUID(), time.Now().UnixMilli(), jsontext.Value(`{"issue_uid":"`+incoming.UID+`","uid":"`+incoming.UID+`"}`))
	_, err = store.InsertRemoteEvents(ctx, busy.ID, []db.RemoteEvent{unrelated})
	require.NoError(t, err)

	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}

	created, err := store.PatchProjectMetadata(ctx, db.PatchProjectMetadataIn{ProjectID: quiet.ID, Actor: "tester", Patch: map[string]jsontext.Value{"purpose": jsontext.Value(`"Local event"`)}})
	require.NoError(t, err)
	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}
	out, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: outgoing.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: outgoing.Revision, Actor: "tester"})
	require.NoError(t, err)
	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}
	in, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: incoming.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: incoming.Revision, Actor: "tester"})
	require.NoError(t, err)
	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}
	away, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: returning.ID, FromProjectID: quiet.ID, ToProjectID: busy.ID, IfMatchRev: returning.Revision, Actor: "tester"})
	require.NoError(t, err)
	returning, err = store.IssueByUID(ctx, returning.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	if beforeHistory != nil {
		beforeHistory(quiet, busy)
	}
	back, err := store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: returning.ID, FromProjectID: busy.ID, ToProjectID: quiet.ID, IfMatchRev: returning.Revision, Actor: "tester"})
	require.NoError(t, err)
	return FederatedReadLifecycle{Project: quiet, Busy: busy, Incoming: incoming, Outgoing: outgoing, AfterID: after, EventIDs: []int64{history.ID, payloadIDs[0], payloadIDs[1], created.Event.ID, out.EventID, in.EventID, away.EventID, back.EventID}, IncomingEventIDs: []int64{history.ID, payloadIDs[0], payloadIDs[1], in.EventID}}
}

// QueryFederatedReadEventIDs decodes the ID-only projection used by page tests.
func QueryFederatedReadEventIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]db.Event, error) {
	return (sqlquery.Query{SQL: query, Args: args}).All(ctx, tx, func(rows *sql.Rows) (db.Event, error) {
		var event db.Event
		err := rows.Scan(&event.ID)
		return event, err
	})
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
 SELECT '00000000000000000000'||CAST(100000+$5+(i.id-$6)*100*$4+x AS TEXT),$1,CASE WHEN x%$4=0 THEN CAST($2 AS BIGINT) ELSE CAST($3 AS BIGINT) END,'source-project',
 CASE WHEN x%$4=0 THEN i.id ELSE NULL END,CASE WHEN x%$4=0 THEN i.uid ELSE NULL END,'issue.updated','tester','{}',1,0,$7
 FROM issues i CROSS JOIN n WHERE i.project_id=$2 ORDER BY i.id,x`, store.InstanceUID(), source.ID, other.ID, stride, after, issues[0].ID, strings.Repeat("a", 64))
	require.NoError(t, err)
	for _, issue := range issues {
		_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: target.ID, IfMatchRev: issue.Revision, Actor: "tester"})
		require.NoError(t, err)
	}
	return target, after
}
