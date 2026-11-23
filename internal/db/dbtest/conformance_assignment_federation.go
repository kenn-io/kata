package dbtest

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

func checkExpiredAssignmentFederationReplay(t *testing.T, hub db.Storage, backend Backend) error {
	ctx := t.Context()
	project, err := hub.CreateProject(ctx, "expiry-replay")
	require.NoError(t, err)
	issue, _, err := hub.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "claimed work", Author: "worker"})
	require.NoError(t, err)
	_, err = hub.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"work.attention": jsontext.Value(`"ok"`)}})
	require.NoError(t, err)
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	_, err = hub.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "worker", TTL: time.Minute, Now: now})
	require.NoError(t, err)
	_, err = hub.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	spoke := backend.Open(t)
	t.Cleanup(func() { require.NoError(t, spoke.Close()) })
	local, err := spoke.CreateProjectWithUID(ctx, project.Name, project.UID)
	require.NoError(t, err)
	_, err = spoke.UpsertFederationBinding(ctx, db.FederationBinding{ProjectID: local.ID, Role: db.FederationRoleSpoke, HubURL: "https://hub.example", HubProjectID: project.ID, HubProjectUID: project.UID, Enabled: true, PushEnabled: true, Actor: "worker"})
	require.NoError(t, err)
	pull := func(after int64) int64 {
		page, err := hub.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, AfterID: after, Limit: 100, Features: db.SupportedEventFeatures})
		require.NoError(t, err)
		var events []db.RemoteEvent
		for _, event := range page.Events {
			events = append(events, remoteMoveEvent(event))
		}
		require.NotEmpty(t, events)
		_, err = spoke.InsertRemoteEvents(ctx, local.ID, events)
		require.NoError(t, err)
		require.NoError(t, spoke.MaterializeFederatedProject(ctx, local.ID))
		return page.NextAfterID
	}
	cursor := pull(0)
	_, err = hub.ExpireAssignments(ctx, db.ExpireAssignmentsParams{ProjectID: project.ID, Now: now.Add(2 * time.Minute)})
	require.NoError(t, err)
	cursor = pull(cursor)
	replayed, err := spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Nil(t, replayed.Owner)
	metadata := db.PayloadMap(jsontext.Value(replayed.Metadata))
	require.JSONEq(t, `"needs-human"`, string(metadata["work.attention"]))
	require.JSONEq(t, `"claim expired without hand-off"`, string(metadata["work.attention_msg"]))
	claim, err := hub.ClaimOwner(ctx, db.ClaimOwnerParams{IssueID: issue.ID, Actor: "next-worker", TTL: time.Minute, Now: now.Add(2 * time.Minute)})
	require.NoError(t, err)
	pull(cursor)
	replayed, err = spoke.IssueByUID(ctx, issue.UID, db.IncludeDeletedNo)
	require.NoError(t, err)
	require.Equal(t, claim.Issue.Owner, replayed.Owner)
	hubMeta := db.PayloadMap(jsontext.Value(claim.Issue.Metadata))
	metadata = db.PayloadMap(jsontext.Value(replayed.Metadata))
	require.Equal(t, hubMeta["work.attention"], metadata["work.attention"])
	require.Equal(t, hubMeta["work.attention_msg"], metadata["work.attention_msg"])
	return nil
}
