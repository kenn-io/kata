package daemon_test

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/federationprovider"
)

func providerMapping(t *testing.T) (config.CatalogDaemonConfig, config.FederationProjectConfig) {
	t.Helper()
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_PROVIDER_PROCESS", "1")
	executable, err := os.Executable()
	require.NoError(t, err)
	return config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"}, config.FederationProjectConfig{
		Hub: "team-hub", SpokeProject: "spoke-project", HubProject: "hub-project", Intent: "collaborate",
		CredentialProvider: []string{executable, "-test.run=^TestFederationProviderProcess$", "--", "literal argument $(not-a-command)"},
	}
}

// This executable fixture is the external provider, not the controller under
// test. It verifies that the request already exists in the real credential file
// before returning a decision. A candidate sent before persistence exits 2.
func TestFederationProviderProcess(_ *testing.T) {
	if os.Getenv("KATA_TEST_PROVIDER_PROCESS") != "1" {
		return
	}
	request, err := federationprovider.DecodeRequest(os.Stdin)
	if err != nil || !slices.Contains(os.Args, "literal argument $(not-a-command)") {
		os.Exit(2)
	}
	entries, err := config.ReadFederationCredentials()
	if err != nil {
		os.Exit(2)
	}
	var saved config.FederationCredential
	for _, credential := range entries.Projects {
		if credential.Provider != nil && credential.Provider.RequestID == request.RequestID {
			saved = credential
		}
	}
	if saved.Provider == nil || saved.Provider.LocalProjectUID != request.LocalProjectUID ||
		saved.Provider.SpokeInstanceUID != request.SpokeInstanceUID || saved.HubProjectName != request.Project ||
		saved.HubURL != request.HubURL || saved.Provider.Intent != request.Intent ||
		(request.Operation == "authorize" && saved.Token != request.CandidateToken) ||
		(request.Operation == "release" && request.CandidateToken != "") {
		os.Exit(2)
	}
	response := federationprovider.Response{Version: 1, Operation: request.Operation, RequestID: request.RequestID, Status: federationprovider.Status(os.Getenv("KATA_TEST_PROVIDER_STATUS"))}
	if response.Status == "ready" {
		response.HubURL = request.HubURL
		response.ProjectID = 42
		response.ProjectUID = replicaHubProjectUID
		response.EnrollmentID = 7
		response.Actor = "Example User"
		if actor := os.Getenv("KATA_TEST_PROVIDER_ACTOR"); actor != "" {
			response.Actor = actor
		}
		response.Capabilities = "claim,pull,push"
		if request.Intent == "read_only" {
			response.Capabilities = "pull"
		}
		response.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if err := federationprovider.WriteResponse(os.Stdout, request, response); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestFederationProviderAttachesEmptyReplicaWithoutAdoption(t *testing.T) {
	catalog, mapping := providerMapping(t)
	mapping.Intent = "read_only"
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "ready")
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	credentials := config.DefaultFederationCredentialStore()
	ready, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	var events []db.Event
	params := daemon.EnsureFederationReplicaParams{
		HubURL: catalog.URL, HubProjectID: 42, HubProjectUID: replicaHubProjectUID,
		ProjectName: project.Name, ReplayHorizonEventID: 9, Credential: ready.Credential,
		AttachEmpty:      true,
		CredentialRekey:  &daemon.FederationReplicaCredentialRekeySource{ProjectUID: ready.ProjectUID, Expected: ready.Credential},
		ProjectEventSink: func(event db.Event) { events = append(events, event) },
	}
	result, err := daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
	require.NoError(t, err)
	assert.False(t, result.Adopted)
	assert.False(t, result.Binding.PushEnabled)
	assert.Equal(t, project.ID, result.Project.ID)
	assert.Equal(t, replicaHubProjectUID, result.Project.UID)
	require.Len(t, events, 1)
	assert.Equal(t, replicaHubProjectUID, events[0].ProjectUID)
	assert.Equal(t, result.CreatedEvent.ID, events[0].ID)
	saved, found, err := credentials.FederationCredential(t.Context(), replicaHubProjectUID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, project.UID, saved.Provider.LocalProjectUID)
	assert.Equal(t, ready.Credential.Provider.RequestID, saved.Provider.RequestID)

	// After binding, replay does not rekey again or publish another event.
	params.CredentialRekey = nil
	_, err = daemon.EnsureFederationReplica(t.Context(), store, credentials, nil, params)
	require.NoError(t, err)
	assert.Len(t, events, 1, "resuming must not create another catalog event")
}

func TestFederationProviderPersistsBeforeContactAndResumes(t *testing.T) {
	catalog, mapping := providerMapping(t)
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "approval_required")
	credentials := config.DefaultFederationCredentialStore()
	peer := config.FederationCredential{HubURL: "https://other.example", Token: "synthetic-peer-credential"}
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), "peer-project", peer))
	pending, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	require.NotNil(t, pending.Credential.Provider)
	assert.Equal(t, federationprovider.StatusApprovalRequired, pending.Credential.Provider.Status)
	assert.Equal(t, project.UID, pending.ProjectUID)
	assert.Empty(t, pending.Credential.Actor)
	assert.Zero(t, pending.Credential.HubProjectID)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound, "pending authorization grants no federation")

	// A new store handle rereads disk, as after a daemon restart. Readying the
	// saved request must not substitute its token or local project identity.
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "ready")
	ready, err := daemon.AuthorizeFederationProvider(t.Context(), store, config.DefaultFederationCredentialStore(), catalog, mapping)
	require.NoError(t, err)
	assert.Equal(t, pending.Credential.Provider.RequestID, ready.Credential.Provider.RequestID)
	assert.True(t, pending.Credential.Token == ready.Credential.Token, "the saved token must not change")
	assert.Equal(t, federationprovider.StatusReady, ready.Credential.Provider.Status)
	assert.Equal(t, int64(42), ready.Credential.HubProjectID)
	assert.Equal(t, int64(7), ready.Credential.Provider.EnrollmentID)
	assert.Equal(t, "2030-01-01T00:00:00Z", ready.Credential.Provider.ExpiresAt.Format(time.RFC3339))

	// Once ready, ordinary local reuse requires no running helper or fresh login.
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "invalid-output")
	replayed, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	assert.True(t, ready.Credential.Equal(replayed.Credential))

	// Binding changes the local project UID to the hub UID. The existing
	// replica service rekeys credentials first; interruption in between must
	// still recover this request, not replace its token.
	require.NoError(t, credentials.RekeyFederationCredential(t.Context(), config.FederationCredentialRekey{
		FromProjectUID: project.UID, ToProjectUID: replicaHubProjectUID,
		Expected: ready.Credential, Replacement: ready.Credential,
	}))
	replayed, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	assert.Equal(t, replicaHubProjectUID, replayed.ProjectUID)
	assert.True(t, ready.Credential.Equal(replayed.Credential))

	changed := mapping
	changed.HubProject = "another-project"
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, changed)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	changed = mapping
	changed.CredentialProvider = append(slices.Clone(mapping.CredentialProvider), "--different-profile")
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, changed)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)

	// Release can fail offline without losing the saved operation. It blocks
	// authorize, and a retry closes only this exact provider-owned operation.
	_, err = daemon.ReleaseFederationProvider(t.Context(), store, credentials, project.ID)
	require.Error(t, err)
	retained, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, retained.Credential.LeavePending)
	_, err = daemon.LeaveFederationReplica(t.Context(), store, credentials, nil, project.ID)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaLeavePending, "ordinary teardown must not discard an unconfirmed provider release")
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaLeavePending)
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "released")
	released, err := daemon.ReleaseFederationProvider(t.Context(), store, credentials, project.ID)
	require.NoError(t, err)
	assert.Equal(t, federationprovider.StatusReleased, released.Credential.Provider.Status)
	assert.Equal(t, pending.Credential.Provider.RequestID, released.Credential.Provider.RequestID)
	_, err = store.ProjectByID(t.Context(), project.ID)
	require.NoError(t, err, "releasing authority does not delete local tasks")
	peerAfter, found, err := credentials.FederationCredential(t.Context(), "peer-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, peer.Equal(peerAfter), "releasing one request must not change another credential")
	_, err = daemon.LeaveFederationReplica(t.Context(), store, credentials, nil, project.ID)
	require.NoError(t, err)
	closed, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found, "closed request prevents a still-configured mapping reopening after restart")
	assert.Empty(t, closed.Credential.Token)
	assert.Equal(t, federationprovider.StatusReleased, closed.Credential.Provider.Status)
}

func TestFederationProviderKeepsFailedAndDeniedRequests(t *testing.T) {
	for _, status := range []string{"invalid-output", "denied", "conflict", "sign_in_required"} {
		t.Run(status, func(t *testing.T) {
			catalog, mapping := providerMapping(t)
			store := openReplicaServiceStore(t)
			_, err := store.CreateProject(t.Context(), mapping.SpokeProject)
			require.NoError(t, err)
			credentials := config.DefaultFederationCredentialStore()
			t.Setenv("KATA_TEST_PROVIDER_STATUS", status)
			_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
			if status == "invalid-output" {
				require.ErrorIs(t, err, federationprovider.ErrProviderFailed)
			} else {
				require.NoError(t, err)
			}
			before, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
			require.NoError(t, err)
			require.True(t, found)
			t.Setenv("KATA_TEST_PROVIDER_STATUS", "ready")
			after, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
			require.NoError(t, err)
			assert.Equal(t, before.Credential.Provider.RequestID, after.Credential.Provider.RequestID)
			assert.True(t, before.Credential.Token == after.Credential.Token)
			if status == "denied" || status == "conflict" {
				assert.Equal(t, federationprovider.Status(status), after.Credential.Provider.Status, "terminal decisions must not restart issuance")
			} else {
				assert.Equal(t, federationprovider.StatusReady, after.Credential.Provider.Status)
			}
		})
	}
}

func TestFederationProviderPreservesUnrelatedCredentials(t *testing.T) {
	catalog, mapping := providerMapping(t)
	store := openReplicaServiceStore(t)
	project, err := store.CreateProject(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	credentials := config.DefaultFederationCredentialStore()
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), project.UID, config.FederationCredential{Token: "existing-owner-credential"}))
	path, err := config.FederationCredentialsPath()
	require.NoError(t, err)
	before, err := os.ReadFile(path) //nolint:gosec // credential path belongs to this test's temporary Kata home
	require.NoError(t, err)
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	after, err := os.ReadFile(path) //nolint:gosec // credential path belongs to this test's temporary Kata home
	require.NoError(t, err)
	assert.True(t, string(before) == string(after), "existing credentials must remain untouched")
}

func TestFederationProviderValidatesActorBeforeSavingReady(t *testing.T) {
	catalog, mapping := providerMapping(t)
	store := openReplicaServiceStore(t)
	_, err := store.CreateProject(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	credentials := config.DefaultFederationCredentialStore()
	t.Setenv("KATA_TEST_PROVIDER_STATUS", "ready")
	t.Setenv("KATA_TEST_PROVIDER_ACTOR", " BOOTSTRAP ")
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.ErrorIs(t, err, federationprovider.ErrProviderFailed)
	pending, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found)
	assert.Empty(t, pending.Credential.Provider.Status)
	assert.Empty(t, pending.Credential.Actor)
	t.Setenv("KATA_TEST_PROVIDER_ACTOR", " Example User ")
	ready, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	assert.Equal(t, "Example User", ready.Credential.Actor)
	assert.Equal(t, pending.Credential.Provider.RequestID, ready.Credential.Provider.RequestID)
	assert.True(t, pending.Credential.Token == ready.Credential.Token)
}
