package daemon_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
)

func TestProviderCredentialSurvivesDaemonReservationAndBinding(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := t.Context()
	store := openReplicaServiceStore(t)
	credentials := config.DefaultFederationCredentialStore()
	params := replicaServiceParams()
	params.HubURL = "https://hub.example/tools/tasks"
	params.Credential.HubURL = params.HubURL
	params.Credential.Capabilities = "claim,pull,push"
	params.Credential.ManagedByConfig = true
	params.Credential.SpokeProjectName = params.ProjectName
	params.Credential.Provider = &config.FederationProviderCredential{
		RequestID: uuid.New(), Command: []string{"example-credential-provider"},
		Intent: "collaborate", SpokeInstanceUID: store.InstanceUID(),
		LocalProjectUID: replicaLocalProjectUID, HubProjectUID: replicaHubProjectUID,
		EnrollmentID: 7, Status: "ready", ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	reservation := config.FederationManagedCredentialReservation{
		ProjectUID: replicaHubProjectUID, Credential: params.Credential,
	}
	require.NoError(t, credentials.ReserveManagedFederationCredential(ctx, reservation))
	finish, err := daemon.BeginFederationReplicaHubOperation(ctx, store, credentials, params.ProjectName, reservation)
	require.NoError(t, err, "same operation must survive decoding its provider metadata")
	left, err := finish(ctx, 7)
	require.NoError(t, err)
	assert.False(t, left)

	params.ManagedReservation = &daemon.FederationReplicaManagedReservation{
		ProjectUID: replicaHubProjectUID, Expected: params.Credential,
	}
	result, err := daemon.EnsureFederationReplica(ctx, store, credentials, nil, params)
	require.NoError(t, err)
	bound, err := store.FederationBindingByProject(ctx, result.Project.ID)
	require.NoError(t, err)
	assert.Equal(t, replicaHubProjectUID, bound.HubProjectUID)
	assert.True(t, bound.PushEnabled)
	saved, found, err := credentials.FederationCredential(ctx, result.Project.UID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, saved.Provider)
	assert.Equal(t, params.Credential.Provider.RequestID, saved.Provider.RequestID)
	assert.Equal(t, int64(7), saved.Provider.EnrollmentID)

	// Manual join callers have no managed reservation. Reusing the same
	// bearer must not discard or replace the provider operation during ensure.
	for _, name := range []string{"manual join", "different operation"} {
		t.Run(name, func(t *testing.T) {
			manual := params
			manual.ManagedReservation = nil
			manual.Credential.Provider = nil
			if name == "different operation" {
				manual.Credential.Provider = new(*saved.Provider)
				manual.Credential.Provider.RequestID = uuid.New()
			}
			_, err := daemon.EnsureFederationReplica(ctx, store, credentials, nil, manual)
			require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
			retained, exists, err := credentials.FederationCredential(ctx, result.Project.UID)
			require.NoError(t, err)
			require.True(t, exists)
			assert.Equal(t, saved, retained)
		})
	}

	// Changing the retained operation cannot reuse the earlier hub-operation
	// fence, even when its token and ordinary binding fields are unchanged.
	changed := saved
	changed.Provider = new(*saved.Provider)
	changed.Provider.RequestID = uuid.New()
	require.NoError(t, credentials.StoreFederationCredential(ctx, result.Project.UID, changed))
	_, err = daemon.BeginFederationReplicaHubOperation(ctx, store, credentials, params.ProjectName,
		config.FederationManagedCredentialReservation{ProjectUID: result.Project.UID, Credential: saved})
	require.ErrorIs(t, err, daemon.ErrFederationReplicaReservationChanged)
}
