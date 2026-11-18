package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func checkSelectedScheduleProjection(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "calendar-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Scheduled", Author: "operator"})
	require.NoError(t, err)
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2027-05-12T00:30[Asia/Tokyo]"`)}})
	require.NoError(t, err)
	snapshot, err := store.(db.UIStore).ReadUISnapshot(ctx, db.UISnapshotQuery{View: "all-open", ProjectUID: project.UID, SelectedIssueUID: issue.UID, TimeZone: "America/New_York", DefaultTimezone: "UTC", Limit: 10})
	require.NoError(t, err)
	require.Len(t, snapshot.Issues, 1)
	require.Equal(t, "2027-05-11", snapshot.Issues[0].ScheduledOnDate)
	require.NotNil(t, snapshot.SelectedIssue)
	require.Equal(t, snapshot.Issues[0].ScheduledOnDate, snapshot.SelectedIssue.ScheduledOnDate)
	return nil
}

func checkDateZoneFederation(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "zone-project")
	require.NoError(t, err)
	issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "Scheduled", Author: "operator"})
	require.NoError(t, err)
	_, err = store.EnableProjectFederation(ctx, project.ID, "operator")
	require.NoError(t, err)
	peer, err := uid.New()
	require.NoError(t, err)
	event := newRemoteEvent(t, project, &issue.UID, "issue.metadata_updated", "operator", peer, 999, jsontext.Value(`{"diff":{"scheduled_on":{"from":null,"to":"2026-01-15[Europe/Paris]"}},"revision_new":2}`))
	before, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.IngestFederationEvents(ctx, db.FederationIngestParams{ProjectID: project.ID, SpokeInstanceUID: peer, BoundActor: "operator", Events: []db.FederationIngestEvent{{SourceEventID: 1, Event: event}}})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	after, err := store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	got, err := store.IssueByID(ctx, issue.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(got.Metadata))
	_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "operator", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-01-15[Europe/Paris]"`)}})
	require.NoError(t, err)
	before, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Limit: 100})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	after, err = store.MaxEventID(ctx)
	require.NoError(t, err)
	require.Equal(t, before, after)
	return nil
}
