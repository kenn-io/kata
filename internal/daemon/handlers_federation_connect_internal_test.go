package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

type bridgeConnectIdentityStore struct{ db.Storage }

func (bridgeConnectIdentityStore) InstanceUID() string { return "00000000000000000000000009" }

func (bridgeConnectIdentityStore) ProjectByNameIncludingArchived(context.Context, string) (db.Project, error) {
	return db.Project{}, db.ErrNotFound
}

func (bridgeConnectIdentityStore) ProjectByUID(context.Context, string) (db.Project, error) {
	return db.Project{}, db.ErrNotFound
}

type bridgeConnectCredentialStore struct {
	credentials map[string]config.FederationCredential
}

type bridgeConnectCredentialStoreWithoutPendingReader struct {
	config.FederationCredentialStore
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

func (s *bridgeConnectCredentialStore) PendingRelayCredentialMetadata(
	_ context.Context,
	projectName string,
) (string, config.FederationCredentialMetadata, bool, error) {
	var projectUID string
	var metadata config.FederationCredentialMetadata
	for uid, credential := range s.credentials {
		if !credential.RelayEnrollmentPending || credential.SpokeProjectName != projectName {
			continue
		}
		if projectUID != "" {
			return "", config.FederationCredentialMetadata{}, false, config.ErrFederationCredentialConflict
		}
		projectUID = uid
		metadata = credential.Metadata()
	}
	return projectUID, metadata, projectUID != "", nil
}

func TestBridgeConnectRejectsArchivedProjectNameBeforeReservation(t *testing.T) {
	ctx := t.Context()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "connect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	_, _, err = store.RemoveProject(ctx, db.RemoveProjectParams{
		ProjectID: project.ID, Actor: "example-actor", Force: true,
	})
	require.NoError(t, err)
	credentials := &bridgeConnectCredentialStore{credentials: make(map[string]config.FederationCredential)}
	body := api.FederationBridgeBody{
		HubCatalog: "example-hub", HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "00000000000000000000000008", ProjectName: project.Name,
		LocalAccount: "example-actor", UpstreamAccount: "upstream-actor",
	}

	_, err = reserveFederationBridgeCredential(ctx, store, credentials, body, false)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "project_name_collision", apiErr.Code)
	_, found, err := credentials.FederationCredential(ctx, body.HubProjectUID)
	require.NoError(t, err)
	require.False(t, found, "an archived name collision must not strand an enrollment candidate")
}

func TestBridgeConnectRejectsConflictingPendingNameBeforeReservation(t *testing.T) {
	ctx := t.Context()
	store := bridgeConnectIdentityStore{}
	credentials := &bridgeConnectCredentialStore{credentials: map[string]config.FederationCredential{
		"00000000000000000000000007": {
			HubURL: "https://other-hub.example", HubProjectID: 7, Token: "other-candidate-token",
			SpokeProjectName: "spoke-project", RelayEnrollmentPending: true,
		},
	}}
	body := api.FederationBridgeBody{
		HubCatalog: "example-hub", HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "00000000000000000000000008", ProjectName: "spoke-project",
		LocalAccount: "example-actor", UpstreamAccount: "upstream-actor",
	}

	_, err := reserveFederationBridgeCredential(ctx, store, credentials, body, false)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "federation_credential_conflict", apiErr.Code)
	_, found, err := credentials.FederationCredential(ctx, body.HubProjectUID)
	require.NoError(t, err)
	require.False(t, found, "a pending candidate for another UID must not be overwritten or duplicated")
	_, found, err = credentials.FederationCredential(ctx, "00000000000000000000000007")
	require.NoError(t, err)
	require.True(t, found, "the existing pending candidate must remain intact")
}

func TestBridgeConnectFailsClosedWithoutPendingNameReader(t *testing.T) {
	ctx := t.Context()
	store := bridgeConnectIdentityStore{}
	backing := &bridgeConnectCredentialStore{credentials: make(map[string]config.FederationCredential)}
	credentials := bridgeConnectCredentialStoreWithoutPendingReader{FederationCredentialStore: backing}
	body := api.FederationBridgeBody{
		HubCatalog: "example-hub", HubURL: "https://hub.example", HubProjectID: 42,
		HubProjectUID: "00000000000000000000000008", ProjectName: "spoke-project",
		LocalAccount: "example-actor", UpstreamAccount: "upstream-actor",
	}

	_, err := reserveFederationBridgeCredential(ctx, store, credentials, body, false)
	var apiErr *api.APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "federation_credentials_unavailable", apiErr.Code)
	_, found, err := backing.FederationCredential(ctx, body.HubProjectUID)
	require.NoError(t, err)
	require.False(t, found, "a missing pending-name lookup must not create an unchecked reservation")
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
