package federation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	"go.kenn.io/kata/pkg/federationprovider"
)

type unavailableProviderProjectStore struct {
	db.Storage
	lookupFailed atomic.Bool
}

func (s *unavailableProviderProjectStore) ProjectByUID(ctx context.Context, uid string) (db.Project, error) {
	if !s.lookupFailed.Swap(true) {
		return db.Project{}, errors.New("database connection interrupted")
	}
	return s.Storage.ProjectByUID(ctx, uid)
}

func TestRemovedProviderCleanupSurvivesPurgedProjectAndDatabaseRetry(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "approval_required")
	store := openReconcileStore(t)
	credentials := config.DefaultFederationCredentialStore()
	executable, err := os.Executable()
	require.NoError(t, err)
	project, err := store.CreateProject(t.Context(), "spoke-project")
	require.NoError(t, err)
	_, err = daemon.AuthorizeFederationProvider(t.Context(), store, credentials,
		config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"},
		config.FederationProjectConfig{Hub: "team-hub", SpokeProject: project.Name, HubProject: "hub-project", Intent: "collaborate", CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"}})
	require.NoError(t, err)
	_, _, err = store.RemoveProject(t.Context(), db.RemoveProjectParams{ProjectID: project.ID, Actor: "Example User"})
	require.NoError(t, err)
	_, err = store.PurgeProject(t.Context(), db.PurgeProjectParams{ProjectID: project.ID, Actor: "Example User"})
	require.NoError(t, err)
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "released")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := newManualClock(time.Now())
	r := federation.NewReconciler(federation.ReconcilerConfig{Store: &unavailableProviderProjectStore{Storage: store}, Credentials: credentials, Clock: clock, Wake: cancel})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return r.Health().LastErrorCategory != "" }, 3*time.Second, 10*time.Millisecond)
	assert.Equal(t, "local_storage", r.Health().LastErrorCategory)
	waitForTimerCount(t, clock, 1)
	clock.Advance(time.Second)
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		require.FailNow(t, "cleanup did not retry the retained request")
	}
	_, found, err := credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestReconcileProviderProcess(_ *testing.T) {
	if os.Getenv("KATA_TEST_RECONCILE_PROVIDER") != "1" {
		return
	}
	request, err := federationprovider.DecodeRequest(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	if os.Getenv("KATA_TEST_PROVIDER_DECISION") == "exit-two" {
		os.Exit(2)
	}
	if request.Operation == "release" {
		saved, found, err := config.FindManagedFederationCredential("spoke-project")
		if err != nil || !found || saved.Credential.Provider == nil || !saved.Credential.LeavePending || saved.Credential.Provider.RequestID != request.RequestID {
			os.Exit(2)
		}
	}
	response := federationprovider.Response{Version: 1, Operation: request.Operation, RequestID: request.RequestID, Status: federationprovider.Status(os.Getenv("KATA_TEST_PROVIDER_DECISION"))}
	if response.Status == "ready" {
		response.HubURL, response.ProjectID, response.ProjectUID = request.HubURL, 42, hubProjectUID
		response.EnrollmentID, response.Actor = 7, "Example User"
		response.Capabilities = "claim,pull,push"
		if os.Getenv("KATA_TEST_PROVIDER_NO_CLAIM") == "1" {
			response.Capabilities = "pull,push"
		}
		if request.Intent == "read_only" {
			response.Capabilities = "pull"
		}
		response.ExpiresAt = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if federationprovider.WriteResponse(os.Stdout, request, response) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestReconcilerRemovesProviderMappingAndRetriesExactCleanup(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
	store := openReconcileStore(t)
	credentials := config.DefaultFederationCredentialStore()
	executable, err := os.Executable()
	require.NoError(t, err)
	catalog := config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"}
	mapping := config.FederationProjectConfig{
		Hub: catalog.Name, SpokeProject: "spoke-project", HubProject: "hub-project", Intent: "collaborate",
		CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
	}
	project, err := store.CreateProject(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	saved, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(t.Context(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke, HubURL: catalog.URL,
		HubProjectID: 42, HubProjectUID: project.UID, ReplayHorizonEventID: 1, Enabled: true,
	})
	require.NoError(t, err)
	peer := config.FederationCredential{HubURL: "https://other.example", Token: "synthetic-peer-token"}
	require.NoError(t, credentials.StoreFederationCredential(t.Context(), "peer-project", peer))
	_, _, _, err = store.RenameProjectAndEvent(t.Context(), project.ID, "renamed-project", "Example User")
	require.NoError(t, err)

	// The process starts with no configured mappings. Its first cleanup attempt
	// cannot reach the provider; cancellation models shutting down that process.
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "unavailable")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := newManualClock(time.Now())
	r := federation.NewReconciler(federation.ReconcilerConfig{
		Store: store, Credentials: credentials, Clock: clock,
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return r.Health().LastErrorCategory != "" }, 3*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	binding, err := store.FederationBindingByProject(t.Context(), project.ID)
	require.NoError(t, err)
	assert.False(t, binding.Enabled)
	pending, found, err := credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, pending.Credential.LeavePending)
	assert.Equal(t, saved.Credential.Provider.RequestID, pending.Credential.Provider.RequestID)
	assert.True(t, saved.Credential.Token == pending.Credential.Token)

	// A new controller and credential handle recover the saved helper/request,
	// without a catalog entry or a new authorization request.
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "released")
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	r = federation.NewReconciler(federation.ReconcilerConfig{
		Store: store, Credentials: config.DefaultFederationCredentialStore(), Wake: cancel,
	})
	require.ErrorIs(t, r.Run(ctx), context.Canceled)
	_, err = store.FederationBindingByProject(t.Context(), project.ID)
	require.ErrorIs(t, err, db.ErrNotFound)
	_, found, err = credentials.FindManagedFederationCredential(t.Context(), project.Name)
	require.NoError(t, err)
	assert.False(t, found)
	_, err = store.ProjectByID(t.Context(), project.ID)
	require.NoError(t, err, "removing a mapping must not remove local tasks")
	peerAfter, found, err := credentials.FederationCredential(t.Context(), "peer-project")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, peer.Equal(peerAfter))
}

func TestReconcileProviderUsesApprovedMetadataWithoutCatalogAdministration(t *testing.T) {
	for _, tc := range []struct {
		name, intent string
		existingData bool
	}{
		{name: "read_only", intent: "read_only"},
		{name: "collaborate", intent: "collaborate"},
		{name: "collaborate without claiming", intent: "collaborate"},
		{name: "migrate", intent: "migrate", existingData: true},
		{name: "collaborate refuses existing data", intent: "collaborate", existingData: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "collaborate without claiming" {
				t.Setenv("KATA_TEST_PROVIDER_NO_CLAIM", "1")
			}
			intent := tc.intent
			t.Setenv("KATA_HOME", t.TempDir())
			t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
			t.Setenv("KATA_TEST_PROVIDER_DECISION", "approval_required")
			store := openReconcileStore(t)
			credentials := config.DefaultFederationCredentialStore()
			metadataCalls := 0
			badMetadata := true
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadataCalls++
				assert.Equal(t, "/tasks/api/v1/projects/42/federation/metadata", r.URL.Path)
				assert.Equal(t, http.MethodGet, r.Method)
				saved, found, err := credentials.FindManagedFederationCredential(r.Context(), "spoke-project")
				if !assert.NoError(t, err) || !assert.True(t, found) {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				assert.Equal(t, federationprovider.StatusReady, saved.Credential.Provider.Status, "ready must be durable before metadata retrieval")
				assert.True(t, r.Header.Get("Authorization") == "Bearer "+saved.Credential.Token, "metadata must use the reserved federation token")
				projectUID := hubProjectUID
				if badMetadata {
					projectUID = recreatedProjectUID
				}
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"project_id": 42, "project_uid": projectUID, "project_name": "hub-project",
					"replay_horizon_event_id": 9, "baseline_through_event_id": 12,
				}))
			}))
			t.Cleanup(server.Close)
			previousTransport := http.DefaultTransport
			http.DefaultTransport = server.Client().Transport
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			executable, err := os.Executable()
			require.NoError(t, err)
			catalog := config.CatalogDaemonConfig{Name: "team-hub", URL: server.URL + "/tasks"}
			mapping := config.FederationProjectConfig{
				Hub: catalog.Name, SpokeProject: "spoke-project", HubProject: "hub-project", Intent: federationprovider.Intent(intent),
				CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
			}
			hub := newFakeHub()
			err = federation.ReconcileMapping(t.Context(), store, credentials, hub, catalog, mapping, nil)
			require.Error(t, err)
			assert.Zero(t, metadataCalls, "pending approval must make no federation request")
			pending, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, federationprovider.StatusApprovalRequired, pending.Credential.Provider.Status)
			if tc.existingData {
				local, err := store.ProjectByName(t.Context(), mapping.SpokeProject)
				require.NoError(t, err)
				_, _, err = store.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: local.ID, Title: "Import this task", Author: "Original Author"})
				require.NoError(t, err)
			}
			t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
			err = federation.ReconcileMapping(t.Context(), store, credentials, hub, catalog, mapping, nil)
			require.ErrorIs(t, err, federation.ErrBindingConflict, "metadata cannot change the approved project")
			badMetadata = false
			// A saved ready result must work even if the helper becomes unavailable.
			t.Setenv("KATA_TEST_PROVIDER_DECISION", "invalid")
			err = federation.ReconcileMapping(t.Context(), store, credentials, hub, catalog, mapping, nil)
			if tc.existingData && intent != "migrate" {
				require.ErrorIs(t, err, federation.ErrBindingConflict)
				project, err := store.ProjectByName(t.Context(), mapping.SpokeProject)
				require.NoError(t, err)
				assert.Equal(t, pending.ProjectUID, project.UID)
				_, err = store.FederationBindingByProject(t.Context(), project.ID)
				require.ErrorIs(t, err, db.ErrNotFound)
				return
			}
			require.NoError(t, err)
			project, err := store.ProjectByName(t.Context(), mapping.SpokeProject)
			require.NoError(t, err)
			assert.Equal(t, hubProjectUID, project.UID)
			binding, err := store.FederationBindingByProject(t.Context(), project.ID)
			require.NoError(t, err)
			assert.Equal(t, intent != "read_only", binding.PushEnabled)
			assert.Equal(t, int64(8), binding.PullCursorEventID)
			saved, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, pending.Credential.Provider.RequestID, saved.Credential.Provider.RequestID)
			assert.True(t, pending.Credential.Token == saved.Credential.Token, "retry must preserve the candidate")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			factoryCalls := 0
			reconciler := federation.NewReconciler(federation.ReconcilerConfig{
				Store: store, Credentials: credentials,
				Targets: []federation.Target{{Catalog: catalog, Mapping: mapping}}, Wake: cancel,
				HubFactory: func(context.Context, config.CatalogDaemonConfig) (federation.Hub, error) {
					factoryCalls++
					return hub, nil
				},
			})
			require.ErrorIs(t, reconciler.Run(ctx), context.Canceled)
			assert.Zero(t, factoryCalls, "provider mappings must not construct an admin client")
			assert.Zero(t, hub.resolveCalls)
			assert.Zero(t, hub.ensureCalls)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)
		})
	}
}

func TestProviderInputErrorIsVisibleAndRetainsExactRequest(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "exit-two")
	store := openReconcileStore(t)
	credentials := config.DefaultFederationCredentialStore()
	executable, err := os.Executable()
	require.NoError(t, err)
	catalog := config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"}
	mapping := config.FederationProjectConfig{
		Hub: catalog.Name, SpokeProject: "spoke-project", HubProject: "hub-project", Intent: "collaborate",
		CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := federation.NewReconciler(federation.ReconcilerConfig{
		Store: store, Credentials: credentials, Targets: []federation.Target{{Catalog: catalog, Mapping: mapping}},
	})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return r.Health().LastErrorCategory != "" }, 3*time.Second, 10*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, "configuration_conflict", r.Health().LastErrorCategory)
	saved, found, err := credentials.FindManagedFederationCredential(t.Context(), mapping.SpokeProject)
	require.NoError(t, err)
	require.True(t, found)
	t.Setenv("KATA_TEST_PROVIDER_DECISION", "approval_required")
	retried, err := daemon.AuthorizeFederationProvider(t.Context(), store, credentials, catalog, mapping)
	require.NoError(t, err)
	assert.Equal(t, saved.Credential.Provider.RequestID, retried.Credential.Provider.RequestID)
	assert.True(t, saved.Credential.Token == retried.Credential.Token)
}

func TestReconcilerStopsTerminalProviderDecisionsWithoutDroppingCleanup(t *testing.T) {
	for _, status := range []string{"denied", "conflict"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
			t.Setenv("KATA_TEST_PROVIDER_DECISION", status)
			store := openReconcileStore(t)
			credentials := config.DefaultFederationCredentialStore()
			executable, err := os.Executable()
			require.NoError(t, err)
			target := federation.Target{
				Catalog: config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"},
				Mapping: config.FederationProjectConfig{
					Hub: "team-hub", SpokeProject: "spoke-project", HubProject: "hub-project", Intent: "collaborate",
					CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
				},
			}
			for range 2 {
				// Restarting reloads the saved decision, not a new authorization.
				func() {
					clock := newManualClock(time.Now())
					r := federation.NewReconciler(federation.ReconcilerConfig{
						Store: store, Credentials: credentials, Targets: []federation.Target{target}, Clock: clock,
					})
					ctx, cancel := context.WithCancel(t.Context())
					done := runReconciler(ctx, t, r)
					defer func() {
						cancel()
						require.ErrorIs(t, <-done, context.Canceled)
					}()
					require.Eventually(t, func() bool { return r.Health().LastErrorCategory == status }, 3*time.Second, time.Millisecond)
					assert.Equal(t, 1, r.Health().Conflicted)
					assert.Zero(t, r.Health().Pending)
					attemptAt := *r.Health().LastAttemptAt
					clock.Advance(24 * time.Hour)
					assert.Never(t, func() bool {
						return !r.Health().LastAttemptAt.Equal(attemptAt) || len(clock.snapshotDurations()) != 0
					}, 20*time.Millisecond, time.Millisecond,
						"terminal authorization must not schedule another attempt")
				}()
				t.Setenv("KATA_TEST_PROVIDER_DECISION", "ready")
			}

			// Removing the mapping still releases the original request. The peer
			// checks that release carries the saved request ID and a leave fence.
			saved, found, err := credentials.FindManagedFederationCredential(t.Context(), target.Mapping.SpokeProject)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, federationprovider.Status(status), saved.Credential.Provider.Status)
			t.Setenv("KATA_TEST_PROVIDER_DECISION", "released")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := federation.NewReconciler(federation.ReconcilerConfig{Store: store, Credentials: credentials, Wake: cancel})
			require.ErrorIs(t, r.Run(ctx), context.Canceled)
			_, found, err = credentials.FindManagedFederationCredential(t.Context(), target.Mapping.SpokeProject)
			require.NoError(t, err)
			assert.False(t, found)
		})
	}
}

func TestReconcilerRetriesWaitingProviderDecisions(t *testing.T) {
	for _, status := range []string{"approval_required", "sign_in_required", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("KATA_TEST_RECONCILE_PROVIDER", "1")
			t.Setenv("KATA_TEST_PROVIDER_DECISION", status)
			store := openReconcileStore(t)
			credentials := config.DefaultFederationCredentialStore()
			executable, err := os.Executable()
			require.NoError(t, err)
			clock := newManualClock(time.Now())
			r := federation.NewReconciler(federation.ReconcilerConfig{
				Store: store, Credentials: credentials, Clock: clock,
				Targets: []federation.Target{{
					Catalog: config.CatalogDaemonConfig{Name: "team-hub", URL: "https://hub.example/tasks"},
					Mapping: config.FederationProjectConfig{
						Hub: "team-hub", SpokeProject: "spoke-project", HubProject: "hub-project", Intent: "collaborate",
						CredentialProvider: []string{executable, "-test.run=^TestReconcileProviderProcess$"},
					},
				}},
			})
			ctx, cancel := context.WithCancel(t.Context())
			done := runReconciler(ctx, t, r)
			defer func() {
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
			}()
			require.Eventually(t, func() bool { return r.Health().LastErrorCategory == status }, 3*time.Second, time.Millisecond)
			waitForTimerCount(t, clock, 1)
			assert.Equal(t, 1, r.Health().Pending)
			saved, found, err := credentials.FindManagedFederationCredential(t.Context(), "spoke-project")
			require.NoError(t, err)
			require.True(t, found)
			t.Setenv("KATA_TEST_PROVIDER_DECISION", "denied")
			clock.Advance(time.Second)
			require.Eventually(t, func() bool { return r.Health().LastErrorCategory == "denied" }, 3*time.Second, time.Millisecond)
			retried, found, err := credentials.FindManagedFederationCredential(t.Context(), "spoke-project")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, saved.Credential.Provider.RequestID, retried.Credential.Provider.RequestID)
			assert.True(t, saved.Credential.Token == retried.Credential.Token)
		})
	}
}
