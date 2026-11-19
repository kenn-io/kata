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
