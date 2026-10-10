package dbtest

import (
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
