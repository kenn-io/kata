package dbtest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/internal/uid"
)

func checkFederatedMoveDelivery(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	projects := make([]db.Project, 3)
	for i, name := range []string{"delivery-source", "delivery-middle", "delivery-target"} {
		var err error
		projects[i], err = store.CreateProject(ctx, name)
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, projects[i].ID, "worker")
		require.NoError(t, err)
	}
	origin, err := uid.New()
	require.NoError(t, err)
	moveEvent := func(issue db.Issue, from, to int, physical int64) db.RemoteEvent {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"issue_uid": issue.UID, "from_project_uid": projects[from].UID, "from_short_id": issue.ShortID, "to_project_uid": projects[to].UID, "to_short_id": issue.ShortID, "updated_at": time.UnixMilli(physical).UTC().Format(time.RFC3339Nano), "snapshot": map[string]string{"uid": issue.UID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"}})
		require.NoError(t, err)
		return newRemoteEvent(t, projects[to], &issue.UID, "issue.moved", "worker", origin, physical, jsontext.Value(payload))
	}
	t.Run("creation earlier in return move batch", func(t *testing.T) {
		issueUID, err := uid.New()
		require.NoError(t, err)
		issueShortID, err := shortid.Derive(issueUID, shortid.MinLength)
		require.NoError(t, err)
		issue := db.Issue{UID: issueUID, ShortID: issueShortID, Title: "round trip"}
		physical := time.Now().UnixMilli()
		payload, err := json.Marshal(map[string]string{"uid": issueUID, "short_id": issue.ShortID, "title": issue.Title, "author": "worker", "status": "open"})
		require.NoError(t, err)
		creation := newRemoteEvent(t, projects[0], &issueUID, "issue.created", "worker", origin, physical, jsontext.Value(payload))
		returned := moveEvent(issue, 1, 0, physical+2)
		result, err := store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[0].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: creation}, {SourceEventID: 3, Event: returned}}})
		require.NoError(t, err)
		require.Equal(t, 2, result.Accepted)
		got, err := store.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, projects[0].ID, got.ProjectID)
	})
	t.Run("predecessor delivery retries", func(t *testing.T) {
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: projects[0].ID, Title: "reverse delivery", Author: "worker"})
		require.NoError(t, err)
		physical := time.Now().Add(time.Second).UnixMilli()
		first, last := moveEvent(issue, 0, 1, physical), moveEvent(issue, 1, 2, physical+1)
		lastParams := db.FederationIngestParams{ProjectID: projects[2].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 5, Event: last}}}
		_, err = store.IngestFederationEvents(ctx, lastParams)
		require.ErrorIs(t, err, db.ErrNotFound, "the predecessor is not an invalid move")
		_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: projects[1].ID, SpokeInstanceUID: origin, BoundActor: "worker", EventFeatures: db.SupportedEventFeatures, Events: []db.FederationIngestEvent{{SourceEventID: 4, Event: first}}})
		require.NoError(t, err)
		_, err = store.IngestFederationEvents(ctx, lastParams)
		require.NoError(t, err)
		got, err := store.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
		require.NoError(t, err)
		require.Equal(t, projects[2].ID, got.ProjectID)
	})
	return nil
}
