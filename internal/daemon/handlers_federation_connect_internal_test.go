package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

type bridgeConnectIdentityStore struct{ db.Storage }

func (bridgeConnectIdentityStore) InstanceUID() string { return "00000000000000000000000009" }

type bridgeConnectCredentialStore struct {
	credentials map[string]config.FederationCredential
}

func (s *bridgeConnectCredentialStore) FederationCredential(_ context.Context, projectUID string) (config.FederationCredential, bool, error) {
	credential, ok := s.credentials[projectUID]
	return credential, ok, nil
}

func (s *bridgeConnectCredentialStore) StoreFederationCredential(_ context.Context, projectUID string, credential config.FederationCredential) error {
	s.credentials[projectUID] = credential
	return nil
}

func (s *bridgeConnectCredentialStore) DeleteFederationCredential(_ context.Context, projectUID string) error {
	delete(s.credentials, projectUID)
	return nil
}

func TestBridgeConnectCanRejoinAfterCompletedLeave(t *testing.T) {
	ctx := t.Context()
	store := bridgeConnectIdentityStore{}
	credentials := &bridgeConnectCredentialStore{credentials: make(map[string]config.FederationCredential)}
	body := api.FederationBridgeBody{
		HubCatalog: "example-hub", HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "00000000000000000000000008", ProjectName: "spoke-project",
		LocalAccount: "example-actor", UpstreamAccount: "upstream-actor",
	}
	credential, err := reserveFederationBridgeCredential(ctx, store, credentials, body, false)
	require.NoError(t, err)
	require.True(t, credential.RelayEnrollmentPending)
	key := federationReplicaOperationKey(store, body.ProjectName, credential)
	ensureFederationReplicaMu.Lock()
	federationReplicaTransitions.markLeft(key)
	ensureFederationReplicaMu.Unlock()
	t.Cleanup(func() {
		ensureFederationReplicaMu.Lock()
		federationReplicaTransitions.clearLeave(key)
		ensureFederationReplicaMu.Unlock()
	})

	finish, err := beginFederationBridgeEnrollment(ctx, store, credentials, body.ProjectName, body.HubProjectUID, credential)
	require.NoError(t, err, "an explicit connect must be able to rejoin after the prior leave completed")
	require.NotNil(t, finish)
	require.Equal(t, federationReplicaIdle, federationReplicaTransitions.state(key), "the rejoin clears completed leave state while registering its in-flight enrollment")
	finish()
	current, found, err := credentials.FederationCredential(ctx, body.HubProjectUID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, current.RelayEnrollmentPending, "the reservation remains retryable until enrollment succeeds")
}

func TestBridgeConnectDoesNotReserveWhileLeaveIsPending(t *testing.T) {
	ctx := t.Context()
	store := bridgeConnectIdentityStore{}
	credentials := &bridgeConnectCredentialStore{credentials: make(map[string]config.FederationCredential)}
	body := api.FederationBridgeBody{
		HubCatalog: "example-hub", HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "00000000000000000000000008", ProjectName: "spoke-project",
		LocalAccount: "example-actor", UpstreamAccount: "upstream-actor",
	}
	key := federationReplicaTransitionKey(store, body.ProjectName)
	ensureFederationReplicaMu.Lock()
	federationReplicaTransitions.markLeavePending(key)
	ensureFederationReplicaMu.Unlock()
	t.Cleanup(func() {
		ensureFederationReplicaMu.Lock()
		federationReplicaTransitions.clearLeave(key)
		ensureFederationReplicaMu.Unlock()
	})

	_, err := reserveFederationBridgeCredential(ctx, store, credentials, body, false)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "federation_credential_conflict", apiErr.Code)
	require.Equal(t, "explicit federation leave is pending", apiErr.Message)
	_, found, readErr := credentials.FederationCredential(ctx, body.HubProjectUID)
	require.NoError(t, readErr)
	require.False(t, found, "a rejected connect must not strand a new pending enrollment credential")
}
