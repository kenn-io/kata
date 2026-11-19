package dbtest

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

// Metadata reads serve only enabled hubs. A spoke binding is a caller error
// that daemons report as federation_not_enabled, not an internal failure.
func checkFederationMetadataRequiresEnabledHub(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	hubUID, err := uid.New()
	require.NoError(t, err)
	_, err = store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{ProjectID: project.ID, HubURL: "https://daemon.example", HubProjectID: 1, HubProjectUID: hubUID, Actor: "worker"})
	require.NoError(t, err)
	_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.CronEventFeature})
	require.ErrorIs(t, err, db.ErrFederationNotEnabled)
	return nil
}

func checkFederationMetadataZonedDateAfterPurge(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "hub-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "retained", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-02[Europe/Paris]"`)}})
	require.NoError(t, err)
	purged, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "purged", Author: "worker"})
	require.NoError(t, err)
	binding, err := store.EnableProjectFederation(ctx, project.ID, "worker")
	require.NoError(t, err)
	_, err = store.PurgeIssue(ctx, purged.ID, "worker", nil)
	require.NoError(t, err)
	_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.CronEventFeature})
	require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
	after, err := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	require.Equal(t, binding.ReplayHorizonEventID, after.ReplayHorizonEventID)
	result, err := store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.CronEventFeature + "," + db.IssueDateZoneEventFeature})
	require.NoError(t, err)
	require.Greater(t, result.Binding.ReplayHorizonEventID, binding.ReplayHorizonEventID)
	return nil
}

func checkFederationMetadataMovedZonedDateHistory(t *testing.T, store db.Storage) error {
	ctx := t.Context()
	for _, tc := range []struct{ name, value string }{{"rewrite", `"2026-09-02"`}, {"clear", `null`}} {
		t.Run(tc.name, func(t *testing.T) {
			destination, err := store.CreateProject(ctx, "destination-"+tc.name)
			require.NoError(t, err)
			source, err := store.CreateProject(ctx, "source-"+tc.name)
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(ctx, source.ID, "worker")
			require.NoError(t, err)
			issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: source.ID, Title: "schedule", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-02[Europe/Paris]"`)}})
			require.NoError(t, err)
			patched, err := store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(tc.value)}})
			require.NoError(t, err)
			_, err = store.EnableProjectFederation(ctx, destination.ID, "worker")
			require.NoError(t, err)
			_, err = store.MoveIssueProject(ctx, db.MoveIssueProjectIn{IssueID: issue.ID, FromProjectID: source.ID, ToProjectID: destination.ID, IfMatchRev: patched.Issue.Revision, Actor: "worker"})
			require.NoError(t, err)
			features := db.CronEventFeature + "," + db.IssueMoveEventFeature
			_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: destination.ID, Metadata: true, Features: features})
			require.ErrorIs(t, err, db.ErrUnsupportedEventFeatures)
			require.Contains(t, err.Error(), db.IssueDateZoneEventFeature)
			_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: destination.ID, Metadata: true, Features: features + "," + db.IssueDateZoneEventFeature})
			require.NoError(t, err)
		})
	}
	t.Run("cleared before enrollment", func(t *testing.T) {
		project, err := store.CreateProject(ctx, "cleared-before-enrollment")
		require.NoError(t, err)
		issue, _, err := store.CreateIssue(ctx, db.CreateIssueParams{ProjectID: project.ID, Title: "schedule", Author: "worker", Metadata: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`"2026-09-02[Europe/Paris]"`)}})
		require.NoError(t, err)
		_, err = store.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: issue.ID, Actor: "worker", Patch: map[string]jsontext.Value{"scheduled_on": jsontext.Value(`null`)}})
		require.NoError(t, err)
		_, err = store.EnableProjectFederation(ctx, project.ID, "worker")
		require.NoError(t, err)
		_, err = store.ReadFederation(ctx, db.FederationReadParams{ProjectID: project.ID, Metadata: true, Features: db.CronEventFeature + "," + db.IssueMoveEventFeature})
		require.NoError(t, err)
	})
	return nil
}
