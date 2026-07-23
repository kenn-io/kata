package daemon_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
)

const (
	replicaHubProjectUID   = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	replicaLocalProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EA"
)

type replicaCredentialStore struct {
	mu          sync.Mutex
	credentials map[string]config.FederationCredential
	readErr     error
	storeErr    error
	deleteErr   error
	rekeyErr    error
	readCalls   int
	storeCalls  int
	deleteCalls int
	rekeyCalls  int
}

type reservationFindingCredentialStore struct {
	*replicaCredentialStore
}

func (s reservationFindingCredentialStore) FederationCredentialReservationForProject(
	_ context.Context, projectName string,
) (config.FederationCredentialReservationMatch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var match config.FederationCredentialReservationMatch
	found := false
	for projectUID, credential := range s.credentials {
		if !credential.ManagedByConfig || credential.SpokeProjectName != projectName {
			continue
		}
		if found && credential != match.Credential {
			return config.FederationCredentialReservationMatch{}, false,
				config.ErrFederationCredentialConflict
		}
		match.Credential = credential
		match.ProjectUIDs = append(match.ProjectUIDs, projectUID)
		found = true
	}
	return match, found, nil
}

func (s reservationFindingCredentialStore) ReserveFederationCredentials(
	_ context.Context, reservation config.FederationCredentialReservation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, projectUID := range reservation.ProjectUIDs {
		if existing, found := s.credentials[projectUID]; found &&
			existing != reservation.Credential {
			return config.ErrFederationCredentialConflict
		}
	}
	for _, projectUID := range reservation.ProjectUIDs {
		s.credentials[projectUID] = reservation.Credential
	}
	return nil
}

func newReplicaCredentialStore() *replicaCredentialStore {
	return &replicaCredentialStore{credentials: make(map[string]config.FederationCredential)}
}

func (s *replicaCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readCalls++
	if s.readErr != nil {
		return config.FederationCredential{}, false, s.readErr
	}
	credential, ok := s.credentials[projectUID]
	return credential, ok, nil
}

func (s *replicaCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential config.FederationCredential,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeCalls++
	if s.storeErr != nil {
		return s.storeErr
	}
	s.credentials[projectUID] = credential
	return nil
}

func (s *replicaCredentialStore) DeleteFederationCredential(_ context.Context, projectUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.credentials, projectUID)
	return nil
}

func (s *replicaCredentialStore) RekeyFederationCredential(
	_ context.Context, rekey config.FederationCredentialRekey,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rekeyCalls++
	if s.rekeyErr != nil {
		return s.rekeyErr
	}
	source, sourceFound := s.credentials[rekey.FromProjectUID]
	target, targetFound := s.credentials[rekey.ToProjectUID]
	if !sourceFound {
		if targetFound && target == rekey.Replacement {
			return nil
		}
		return config.ErrFederationCredentialConflict
	}
	if source != rekey.Expected ||
		(targetFound && target != rekey.Expected && target != rekey.Replacement) {
		return config.ErrFederationCredentialConflict
	}
	s.credentials[rekey.ToProjectUID] = rekey.Replacement
	delete(s.credentials, rekey.FromProjectUID)
	return nil
}

func (s *replicaCredentialStore) put(projectUID string, credential config.FederationCredential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.credentials[projectUID] = credential
}

func (s *replicaCredentialStore) storeCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.storeCalls
}

func (s *replicaCredentialStore) setDeleteError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

func (s *replicaCredentialStore) deleteCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteCalls
}

type replicaCredentialStoreBarrier struct {
	*replicaCredentialStore
	storeStarted chan struct{}
	storeRelease chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
}

func newReplicaCredentialStoreBarrier() *replicaCredentialStoreBarrier {
	return &replicaCredentialStoreBarrier{
		replicaCredentialStore: newReplicaCredentialStore(),
		storeStarted:           make(chan struct{}),
		storeRelease:           make(chan struct{}),
	}
}

func (s *replicaCredentialStoreBarrier) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	s.startOnce.Do(func() { close(s.storeStarted) })
	select {
	case <-s.storeRelease:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.replicaCredentialStore.StoreFederationCredential(ctx, projectUID, credential)
}

func (s *replicaCredentialStoreBarrier) releaseStore() {
	s.releaseOnce.Do(func() { close(s.storeRelease) })
}

type replicaBindingReadBarrierStore struct {
	db.Storage
	arrivals chan struct{}
	release  chan struct{}
	calls    atomic.Int64
}

func newReplicaBindingReadBarrierStore(store db.Storage) *replicaBindingReadBarrierStore {
	return &replicaBindingReadBarrierStore{
		Storage:  store,
		arrivals: make(chan struct{}, 2),
		release:  make(chan struct{}),
	}
}

func (s *replicaBindingReadBarrierStore) FederationBindingByProject(
	ctx context.Context, projectID int64,
) (db.FederationBinding, error) {
	binding, err := s.Storage.FederationBindingByProject(ctx, projectID)
	if s.calls.Add(1) <= 2 {
		s.arrivals <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return db.FederationBinding{}, ctx.Err()
		}
	}
	return binding, err
}

func TestEnsureFederationReplicaCreatesProjectBindingAndCredential(t *testing.T) {
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	wakes := 0

	result, err := daemon.EnsureFederationReplica(
		context.Background(), store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.NoError(t, err)
	assert.Equal(t, "hub-project", result.Project.Name)
	assert.Equal(t, replicaHubProjectUID, result.Project.UID)
	assert.Equal(t, db.FederationRoleSpoke, result.Binding.Role)
	assert.Equal(t, int64(42), result.Binding.HubProjectID)
	assert.Equal(t, replicaHubProjectUID, result.Binding.HubProjectUID)
	assert.Equal(t, int64(9), result.Binding.ReplayHorizonEventID)
	assert.Equal(t, int64(8), result.Binding.PullCursorEventID)
	assert.True(t, result.Binding.PushEnabled)
	assert.False(t, result.Adopted)
	assert.Zero(t, result.AdoptionSnapshotCount)
	assert.Equal(t, 1, wakes)

	stored, ok, err := credentials.FederationCredential(context.Background(), result.Project.UID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "pull,push", stored.Capabilities)
	assert.Equal(t, "enrollment-token", stored.Token)
	assert.False(t, stored.ManagedByConfig)
	assert.Empty(t, stored.HubCatalog)
	assert.Empty(t, stored.HubProjectName)
	assert.Empty(t, stored.RequestedActor)
	assert.Equal(t, 1, credentials.storeCallCount())
}

func TestEnsureFederationReplicaPreservesHubURLPathPrefix(t *testing.T) {
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	params := replicaServiceParams()
	params.HubURL = "https://daemon.example/kata/hub/"
	params.Credential.HubURL = params.HubURL

	result, err := daemon.EnsureFederationReplica(
		context.Background(), store, credentials, nil, params,
	)

	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example/kata/hub", result.Binding.HubURL)
	stored, found, err := credentials.FederationCredential(
		context.Background(), result.Project.UID,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "https://daemon.example/kata/hub", stored.HubURL)
}

func TestEnsureFederationReplicaPreservesEscapedHubURLPathPrefix(t *testing.T) {
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	params := replicaServiceParams()
	params.HubURL = "https://daemon.example/kata%2Fhub/"
	params.Credential.HubURL = params.HubURL

	result, err := daemon.EnsureFederationReplica(
		context.Background(), store, credentials, nil, params,
	)

	require.NoError(t, err)
	assert.Equal(t, "https://daemon.example/kata%2Fhub", result.Binding.HubURL)
	stored, found, err := credentials.FederationCredential(
		context.Background(), result.Project.UID,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "https://daemon.example/kata%2Fhub", stored.HubURL)

	downstreamURL, err := url.JoinPath(
		stored.HubURL,
		"/api/v1/federation/enrollments",
	)
	require.NoError(t, err)
	assert.Equal(
		t,
		"https://daemon.example/kata%2Fhub/api/v1/federation/enrollments",
		downstreamURL,
	)
}

func TestEnsureFederationReplicaRejectsUnsafeHubURLComponents(t *testing.T) {
	for _, tt := range []struct {
		name    string
		hubURL  string
		wantErr bool
		marker  string
	}{
		{
			name:    "user info",
			hubURL:  "https://planted-user@daemon.example/kata/hub",
			wantErr: true,
			marker:  "planted-user",
		},
		{
			name:    "query",
			hubURL:  "https://daemon.example/kata/hub?planted-query=1",
			wantErr: true,
			marker:  "planted-query",
		},
		{
			name:    "fragment",
			hubURL:  "https://daemon.example/kata/hub#planted-fragment",
			wantErr: true,
			marker:  "planted-fragment",
		},
		{
			name:    "empty query delimiter",
			hubURL:  "https://daemon.example/kata/hub?",
			wantErr: true,
			marker:  "?",
		},
		{
			name:    "empty fragment delimiter",
			hubURL:  "https://daemon.example/kata/hub#",
			wantErr: true,
			marker:  "#",
		},
		{
			name:   "clean path prefix",
			hubURL: "https://daemon.example/kata/hub/",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openReplicaServiceStore(t)
			credentials := newReplicaCredentialStore()
			params := replicaServiceParams()
			params.HubURL = tt.hubURL
			params.Credential.HubURL = tt.hubURL

			result, err := daemon.EnsureFederationReplica(
				context.Background(), store, credentials, nil, params,
			)

			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, daemon.ErrFederationReplicaInvalidInput)
				assert.NotContains(t, err.Error(), tt.marker)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "https://daemon.example/kata/hub", result.Binding.HubURL)
		})
	}
}

func TestEnsureFederationReplicaRejectsMalformedHubOrigins(t *testing.T) {
	for _, tt := range []struct {
		name   string
		hubURL string
		marker string
	}{
		{
			name:   "empty hostname with port",
			hubURL: "https://:443/kata/hub",
			marker: ":443",
		},
		{
			name:   "invalid port",
			hubURL: "https://daemon.example:not-a-port/kata/hub",
			marker: "not-a-port",
		},
		{
			name:   "zero port",
			hubURL: "https://daemon.example:0/kata/hub",
			marker: ":0",
		},
		{
			name:   "out of range port",
			hubURL: "https://daemon.example:65536/kata/hub",
			marker: "65536",
		},
		{
			name:   "missing hostname",
			hubURL: "https:///kata/hub",
			marker: "kata/hub",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openReplicaServiceStore(t)
			credentials := newReplicaCredentialStore()
			params := replicaServiceParams()
			params.HubURL = tt.hubURL
			params.Credential.HubURL = tt.hubURL

			_, err := daemon.EnsureFederationReplica(
				context.Background(), store, credentials, nil, params,
			)

			require.Error(t, err)
			assert.ErrorIs(t, err, daemon.ErrFederationReplicaInvalidInput)
			assert.NotContains(t, err.Error(), tt.marker)
			projects, listErr := store.ListProjects(context.Background())
			require.NoError(t, listErr)
			assert.Empty(t, projects)
			assert.Zero(t, credentials.storeCallCount())
		})
	}
}

func TestLeaveFederationReplicaCredentialCleanupFailureResumesWithoutExtraWake(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	created, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, nil, replicaServiceParams(),
	)
	require.NoError(t, err)
	credentials.setDeleteError(errors.New("credential cleanup unavailable"))
	wakes := 0

	_, err = daemon.LeaveFederationReplica(
		ctx, store, credentials, func() { wakes++ }, created.Project.ID,
	)

	require.ErrorContains(t, err, "credential cleanup unavailable")
	_, bindingErr := store.FederationBindingByProject(ctx, created.Project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	_, found, credentialErr := credentials.FederationCredential(ctx, created.Project.UID)
	require.NoError(t, credentialErr)
	assert.True(t, found)
	assert.Equal(t, 1, credentials.deleteCallCount())
	assert.Zero(t, wakes)

	credentials.setDeleteError(nil)
	resumed, err := daemon.LeaveFederationReplica(
		ctx, store, credentials, func() { wakes++ }, created.Project.ID,
	)

	require.NoError(t, err)
	assert.Empty(t, resumed.Role)
	_, found, credentialErr = credentials.FederationCredential(ctx, created.Project.UID)
	require.NoError(t, credentialErr)
	assert.False(t, found)
	assert.Equal(t, 2, credentials.deleteCallCount())
	assert.Equal(t, 1, wakes)
}

func TestLeaveFederationReplicaCleansManagedReservationAliases(t *testing.T) {
	tests := []struct {
		name string
		keys []string
	}{
		{name: "pre-adoption L and H aliases", keys: []string{replicaLocalProjectUID, replicaHubProjectUID}},
		{name: "H-only rekey crash", keys: []string{replicaHubProjectUID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			ctx := context.Background()
			store := openReplicaServiceStore(t)
			project, err := store.CreateProjectWithUID(
				ctx, "spoke-project", replicaLocalProjectUID,
			)
			require.NoError(t, err)
			reservation := replicaServiceParams().Credential
			reservation.ManagedByConfig = true
			reservation.SpokeProjectName = project.Name
			require.NoError(t, config.ReserveFederationCredentials(
				config.FederationCredentialReservation{
					ProjectUIDs: tt.keys,
					Credential:  reservation,
				},
			))
			wakes := 0

			_, err = daemon.LeaveFederationReplica(
				ctx, store, config.DefaultFederationCredentialStore(),
				func() { wakes++ }, project.ID,
			)

			require.NoError(t, err)
			credentials, readErr := config.ReadFederationCredentials()
			require.NoError(t, readErr)
			assert.Empty(t, credentials.Projects)
			assert.Equal(t, 1, wakes)
		})
	}
}

func TestLeaveFederationReplicaRetainsConflictingManualCurrentUIDCredential(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(
		ctx, "spoke-project", replicaLocalProjectUID,
	)
	require.NoError(t, err)
	reservation := replicaServiceParams().Credential
	reservation.Token = "pending-token-a"
	reservation.ManagedByConfig = true
	reservation.SpokeProjectName = project.Name
	require.NoError(t, config.WriteFederationCredential(
		replicaHubProjectUID, reservation,
	))
	manual := replicaServiceParams().Credential
	manual.Token = "manual-token-b"
	require.NoError(t, config.WriteFederationCredential(project.UID, manual))
	wakes := 0

	_, err = daemon.LeaveFederationReplica(
		ctx, store, config.DefaultFederationCredentialStore(),
		func() { wakes++ }, project.ID,
	)

	require.Error(t, err)
	credentials, readErr := config.ReadFederationCredentials()
	require.NoError(t, readErr)
	assert.Equal(t, manual, credentials.Projects[project.UID])
	assert.Equal(t, reservation, credentials.Projects[replicaHubProjectUID])
	assert.Zero(t, wakes)
}

func TestLeaveFederationReplicaWakeRunsOutsideServiceLock(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	created, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, nil, replicaServiceParams(),
	)
	require.NoError(t, err)
	nestedDone := make(chan error, 1)
	leaveDone := make(chan error, 1)

	go func() {
		_, leaveErr := daemon.LeaveFederationReplica(
			ctx, store, credentials, func() {
				_, nestedErr := daemon.LeaveFederationReplica(
					ctx, store, credentials, nil, created.Project.ID,
				)
				nestedDone <- nestedErr
			}, created.Project.ID,
		)
		leaveDone <- leaveErr
	}()

	select {
	case err := <-nestedDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("wake callback deadlocked on the federation replica service lock")
	}
	select {
	case err := <-leaveDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("leave did not return after its wake callback")
	}
}

func TestEnsureFederationReplicaAdoptsStandaloneProjectWithSnapshot(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local issue",
		Author:    "local-author",
	})
	require.NoError(t, err)
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	wakes := 0

	result, err := daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, params)

	require.NoError(t, err)
	assert.Equal(t, project.ID, result.Project.ID)
	assert.Equal(t, replicaHubProjectUID, result.Project.UID)
	assert.True(t, result.Adopted)
	assert.Equal(t, int64(1), result.AdoptionSnapshotCount)
	assert.True(t, result.Binding.PushEnabled)
	assert.Equal(t, 1, wakes)
	events, err := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "issue.snapshot", events[0].Type)
	assert.Equal(t, "sync-agent", events[0].Actor)
}

func TestEnsureFederationReplicaRejectsDistinctCredentialAtAdoptionSource(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	source := replicaServiceParams().Credential
	source.Token = "token-a"
	credentials.put(project.UID, source)
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	params.Credential.Token = "token-b"
	wakes := 0

	_, err = daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, params,
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	unchanged, projectErr := store.ProjectByID(ctx, project.ID)
	require.NoError(t, projectErr)
	assert.Equal(t, replicaLocalProjectUID, unchanged.UID)
	_, bindingErr := store.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	stored, ok, credentialErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, credentialErr)
	require.True(t, ok)
	assert.Equal(t, "token-a", stored.Token)
	_, ok, credentialErr = credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, credentialErr)
	assert.False(t, ok)
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaRejectsManualAdoptionConflictingWithManagedReservation(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	delegate := newReplicaCredentialStore()
	reservation := replicaServiceParams().Credential
	reservation.Token = "pending-token-a"
	reservation.ManagedByConfig = true
	reservation.SpokeProjectName = project.Name
	delegate.put(replicaHubProjectUID, reservation)
	credentials := reservationFindingCredentialStore{replicaCredentialStore: delegate}
	params := replicaServiceParams()
	params.HubProjectID = 43
	params.HubProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
	params.ProjectName = project.Name
	params.AdoptExisting = true
	params.Credential.HubProjectID = 43
	params.Credential.Token = "manual-token-b"
	wakes := 0

	_, err = daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, params,
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	unchanged, projectErr := store.ProjectByID(ctx, project.ID)
	require.NoError(t, projectErr)
	assert.Equal(t, replicaLocalProjectUID, unchanged.UID)
	_, bindingErr := store.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	stored, found, credentialErr := credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, credentialErr)
	require.True(t, found)
	assert.Equal(t, reservation, stored)
	_, found, credentialErr = credentials.FederationCredential(ctx, params.HubProjectUID)
	require.NoError(t, credentialErr)
	assert.False(t, found)
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaAcceptsManagedReservationAtCanonicalOrigin(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	delegate := newReplicaCredentialStore()
	reservation := replicaServiceParams().Credential
	reservation.HubURL = "HTTP://HUB.EXAMPLE:00080/configured/base"
	reservation.ManagedByConfig = true
	reservation.SpokeProjectName = "hub-project"
	delegate.put(replicaHubProjectUID, reservation)
	credentials := reservationFindingCredentialStore{replicaCredentialStore: delegate}
	params := replicaServiceParams()
	params.HubURL = "http://hub.example/manual/base/"
	params.Credential.HubURL = params.HubURL

	result, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, nil, params,
	)

	require.NoError(t, err)
	assert.Equal(t, "http://hub.example/manual/base", result.Binding.HubURL)
}

func TestReserveFederationReplicaCredentialRejectsRecreatedHubAgainstManagedReservation(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	delegate := newReplicaCredentialStore()
	reservation := replicaServiceParams().Credential
	reservation.Token = "pending-token-a"
	reservation.ManagedByConfig = true
	reservation.SpokeProjectName = project.Name
	delegate.put(replicaHubProjectUID, reservation)
	credentials := reservationFindingCredentialStore{replicaCredentialStore: delegate}
	recreatedUID := "01HZNQ7VFPK1XGD8R5MABCD4EY"

	err = daemon.ReserveFederationReplicaCredential(
		ctx, store, credentials,
		daemon.ReserveFederationReplicaCredentialParams{
			HubProjectUID: recreatedUID,
			ProjectName:   project.Name,
			Credential:    reservation,
		},
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	_, found, readErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, readErr)
	assert.False(t, found)
	final, found, readErr := credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, readErr)
	require.True(t, found)
	assert.Equal(t, reservation, final)
	_, found, readErr = credentials.FederationCredential(ctx, recreatedUID)
	require.NoError(t, readErr)
	assert.False(t, found)
}

func TestEnsureFederationReplicaRekeysCredentialBeforeAdoption(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	source := replicaServiceParams().Credential
	source.Token = "token-a"
	credentials.put(project.UID, source)
	replacement := source
	replacement.Actor = "identity-user"
	replacement.ManagedByConfig = true
	replacement.HubCatalog = "primary"
	replacement.HubProjectName = "hub-project"
	wantReplacement := replacement
	wantReplacement.Capabilities = "pull,push"
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	params.Credential = replacement
	params.CredentialRekey = &daemon.FederationReplicaCredentialRekeySource{
		ProjectUID: project.UID,
		Expected:   source,
	}

	result, err := daemon.EnsureFederationReplica(ctx, store, credentials, nil, params)

	require.NoError(t, err)
	assert.True(t, result.Adopted)
	assert.Equal(t, replicaHubProjectUID, result.Project.UID)
	_, ok, credentialErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, credentialErr)
	assert.False(t, ok)
	stored, ok, credentialErr := credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, credentialErr)
	require.True(t, ok)
	assert.Equal(t, wantReplacement, stored)
	assert.Equal(t, 1, credentials.rekeyCalls)
}

func TestEnsureFederationReplicaClassifiesAtomicRekeyConflictWithoutMutation(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	credentials := newReplicaCredentialStore()
	source := replicaServiceParams().Credential
	source.Token = "source-token"
	credentials.put(project.UID, source)
	credentials.rekeyErr = config.ErrFederationCredentialConflict
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	params.Credential = source
	params.CredentialRekey = &daemon.FederationReplicaCredentialRekeySource{
		ProjectUID: project.UID,
		Expected:   source,
	}
	wakes := 0

	_, err = daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, params,
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	unchanged, projectErr := store.ProjectByID(ctx, project.ID)
	require.NoError(t, projectErr)
	assert.Equal(t, replicaLocalProjectUID, unchanged.UID)
	_, bindingErr := store.FederationBindingByProject(ctx, project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	stored, found, credentialErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, credentialErr)
	require.True(t, found)
	assert.Equal(t, source, stored)
	_, found, credentialErr = credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, credentialErr)
	assert.False(t, found)
	assert.Equal(t, 1, credentials.rekeyCalls)
	assert.Zero(t, credentials.storeCallCount())
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaRejectsReplacementOfManagedTargetCredential(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	managed := replicaServiceParams().Credential
	managed.Token = "token-a"
	managed.ManagedByConfig = true
	managed.HubCatalog = "primary"
	managed.HubProjectName = "hub-project"
	credentials.put(replicaHubProjectUID, managed)
	params := replicaServiceParams()
	params.Credential.Token = "token-b"
	wakes := 0

	_, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, params,
	)

	require.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	projects, listErr := store.ListProjects(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, projects)
	stored, ok, credentialErr := credentials.FederationCredential(ctx, replicaHubProjectUID)
	require.NoError(t, credentialErr)
	require.True(t, ok)
	assert.Equal(t, managed, stored)
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaMatchingBindingIsIdempotentAcrossCanonicalOrigins(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	wakes := 0
	first := replicaServiceParams()
	first.HubURL = " HTTP://HUB.EXAMPLE:00080/api/v1 "
	first.Credential.HubURL = first.HubURL
	first.PushEnabled = false

	created, err := daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, first)
	require.NoError(t, err)

	retry := first
	retry.HubURL = " http://hub.example/another/path "
	retry.Credential.HubURL = retry.HubURL
	retry.Credential.Token = "replacement-token"
	got, err := daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, retry)

	require.NoError(t, err)
	assert.Equal(t, created.Project.ID, got.Project.ID)
	assert.Equal(t, int64(8), got.Binding.PullCursorEventID)
	assert.Equal(t, "http://hub.example/another/path", got.Binding.HubURL)
	assert.False(t, got.Adopted)
	assert.Equal(t, 2, wakes, "each successful ensure must wake exactly once")
	projects, err := store.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	stored, ok, err := credentials.FederationCredential(ctx, got.Project.UID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "replacement-token", stored.Token)
	assert.Equal(t, "http://hub.example/another/path", stored.HubURL)
	persisted, err := store.FederationBindingByProject(ctx, got.Project.ID)
	require.NoError(t, err)
	assert.Equal(t, "http://hub.example/another/path", persisted.HubURL)

	downstreamURL, err := url.JoinPath(
		persisted.HubURL,
		"/api/v1/federation/enrollments",
	)
	require.NoError(t, err)
	assert.Equal(t, "http://hub.example/another/path/api/v1/federation/enrollments", downstreamURL)
}

func TestEnsureFederationReplicaWakeMayReenterAfterCompletedState(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	params := replicaServiceParams()
	type ensureOutcome struct {
		result daemon.EnsureFederationReplicaResult
		err    error
	}
	type completedState struct {
		project       db.Project
		projectErr    error
		binding       db.FederationBinding
		bindingErr    error
		credential    config.FederationCredential
		credentialOK  bool
		credentialErr error
	}
	callbackEntered := make(chan struct{})
	stateObserved := make(chan completedState, 1)
	innerDone := make(chan ensureOutcome, 1)
	outerDone := make(chan ensureOutcome, 1)
	var wakes atomic.Int64

	go func() {
		result, err := daemon.EnsureFederationReplica(ctx, store, credentials, func() {
			wakes.Add(1)
			close(callbackEntered)
			state := completedState{}
			state.project, state.projectErr = store.ProjectByUID(ctx, params.HubProjectUID)
			if state.projectErr == nil {
				state.binding, state.bindingErr = store.FederationBindingByProject(ctx, state.project.ID)
				state.credential, state.credentialOK, state.credentialErr =
					credentials.FederationCredential(ctx, state.project.UID)
			}
			stateObserved <- state

			innerResult, innerErr := daemon.EnsureFederationReplica(
				ctx, store, credentials, nil, params,
			)
			innerDone <- ensureOutcome{result: innerResult, err: innerErr}
		}, params)
		outerDone <- ensureOutcome{result: result, err: err}
	}()

	select {
	case <-callbackEntered:
	case <-ctx.Done():
		require.FailNow(t, "wait for wake callback entry", "error: %v", ctx.Err())
	}
	var state completedState
	select {
	case state = <-stateObserved:
	case <-ctx.Done():
		require.FailNow(t, "wait for completed state observation", "error: %v", ctx.Err())
	}
	require.NoError(t, state.projectErr)
	require.NoError(t, state.bindingErr)
	require.NoError(t, state.credentialErr)
	require.True(t, state.credentialOK)
	assert.Equal(t, params.HubProjectUID, state.project.UID)
	assert.Equal(t, params.HubURL, state.binding.HubURL)
	assert.Equal(t, params.HubProjectID, state.binding.HubProjectID)
	assert.True(t, state.binding.PushEnabled)
	assert.Equal(t, params.Credential.HubURL, state.credential.HubURL)
	assert.Equal(t, params.Credential.HubProjectID, state.credential.HubProjectID)
	assert.Equal(t, params.Credential.Token, state.credential.Token)

	var inner ensureOutcome
	select {
	case inner = <-innerDone:
	case <-ctx.Done():
		require.FailNow(t, "re-entrant ensure deadlocked in wake callback", "error: %v", ctx.Err())
	}
	require.NoError(t, inner.err)
	assert.Equal(t, state.project.ID, inner.result.Project.ID)
	assert.True(t, inner.result.Binding.PushEnabled)

	var outer ensureOutcome
	select {
	case outer = <-outerDone:
	case <-ctx.Done():
		require.FailNow(t, "outer ensure did not return after wake callback", "error: %v", ctx.Err())
	}
	require.NoError(t, outer.err)
	assert.Equal(t, state.project.ID, outer.result.Project.ID)
	assert.True(t, outer.result.Binding.PushEnabled)
	assert.Equal(t, int64(1), wakes.Load())
}

func TestEnsureFederationReplicaConcurrentDifferentHubJoinsStayConsistent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sqlStore := openReplicaServiceStore(t)
	project, err := sqlStore.CreateProjectWithUID(ctx, "hub-project", replicaHubProjectUID)
	require.NoError(t, err)
	store := newReplicaBindingReadBarrierStore(sqlStore)
	credentials := newReplicaCredentialStore()
	first := replicaServiceParams()
	first.PushEnabled = false
	first.Credential.Capabilities = "pull"
	first.HubURL = "https://hub-a.example/api"
	first.Credential.HubURL = first.HubURL
	first.Credential.Token = "token-a"
	second := first
	second.HubURL = "https://hub-b.example/api"
	second.HubProjectID = 73
	second.Credential.HubURL = second.HubURL
	second.Credential.HubProjectID = second.HubProjectID
	second.Credential.Token = "token-b"
	var wakes atomic.Int64
	type callResult struct {
		params daemon.EnsureFederationReplicaParams
		result daemon.EnsureFederationReplicaResult
		err    error
	}
	results := make(chan callResult, 2)
	start := make(chan struct{})
	for _, params := range []daemon.EnsureFederationReplicaParams{first, second} {
		go func() {
			<-start
			result, callErr := daemon.EnsureFederationReplica(
				ctx, store, credentials, func() { wakes.Add(1) }, params,
			)
			results <- callResult{params: params, result: result, err: callErr}
		}()
	}
	close(start)
	for range 2 {
		select {
		case <-store.arrivals:
		case result := <-results:
			require.FailNow(
				t,
				"ensure completed before both calls observed the unbound project",
				"error: %v", result.err,
			)
		case <-ctx.Done():
			require.FailNow(t, "wait for concurrent binding reads", "error: %v", ctx.Err())
		}
	}
	close(store.release)

	got := [2]callResult{}
	for i := range got {
		select {
		case got[i] = <-results:
		case <-ctx.Done():
			require.FailNow(t, "wait for concurrent ensure results", "error: %v", ctx.Err())
		}
	}
	var winner *callResult
	failures := 0
	for i := range got {
		if got[i].err == nil {
			require.Nil(t, winner, "exactly one different-hub ensure may succeed")
			winner = &got[i]
			continue
		}
		failures++
		assert.ErrorIs(
			t, got[i].err, daemon.ErrFederationReplicaBindingConflict,
			"post-lock revalidation must reject the loser at the binding boundary",
		)
	}
	require.NotNil(t, winner)
	assert.Equal(t, 1, failures)
	assert.Equal(t, int64(1), wakes.Load())

	binding, err := sqlStore.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, err)
	stored, ok, err := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, winner.params.HubURL, binding.HubURL)
	assert.Equal(t, winner.params.HubProjectID, binding.HubProjectID)
	assert.Equal(t, winner.params.HubURL, stored.HubURL)
	assert.Equal(t, winner.params.HubProjectID, stored.HubProjectID)
	assert.Equal(t, winner.params.Credential.Token, stored.Token)
}

func TestEnsureFederationReplicaRejectsPushCredentialBeforeBindingMutation(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	var wakes int
	created, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)
	require.NoError(t, err)
	beforeBinding, err := store.FederationBindingByProject(ctx, created.Project.ID)
	require.NoError(t, err)
	beforeCredential, ok, err := credentials.FederationCredential(ctx, created.Project.UID)
	require.NoError(t, err)
	require.True(t, ok)
	beforeStoreCalls := credentials.storeCallCount()
	wakes = 0
	invalid := replicaServiceParams()
	invalid.HubURL = "HTTP://HUB.EXAMPLE:80/rewritten"
	invalid.Credential.HubURL = invalid.HubURL
	invalid.Credential.Token = "pull-only-token"
	invalid.Credential.Capabilities = "pull"
	invalid.Credential.AllowInsecure = true
	invalid.PushEnabled = false

	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, invalid)

	require.Error(t, err)
	assert.ErrorIs(t, err, daemon.ErrFederationReplicaInvalidInput)
	afterBinding, bindErr := store.FederationBindingByProject(ctx, created.Project.ID)
	require.NoError(t, bindErr)
	assert.Equal(t, beforeBinding, afterBinding)
	afterCredential, ok, credentialErr := credentials.FederationCredential(ctx, created.Project.UID)
	require.NoError(t, credentialErr)
	require.True(t, ok)
	assert.Equal(t, beforeCredential, afterCredential)
	assert.Equal(t, beforeStoreCalls, credentials.storeCallCount())
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaCredentialReadFailureDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	injected := errors.New("injected credential read failure")
	credentials.readErr = injected
	wakes := 0

	_, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.ErrorIs(t, err, injected)
	projects, listErr := store.ListProjects(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, projects)
	assert.Zero(t, credentials.storeCallCount())
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaCredentialWriteFailureRetriesBinding(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	injected := errors.New("injected credential write failure")
	credentials.storeErr = injected
	wakes := 0

	_, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.ErrorIs(t, err, injected)
	project, projectErr := store.ProjectByUID(ctx, replicaHubProjectUID)
	require.NoError(t, projectErr)
	binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, bindErr)
	assert.False(t, binding.PushEnabled)
	_, ok, credentialErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, credentialErr)
	assert.False(t, ok)
	assert.Zero(t, wakes)

	credentials.storeErr = nil
	result, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.NoError(t, err)
	assert.True(t, result.Binding.PushEnabled)
	_, ok, err = credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, wakes)
	projects, err := store.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
}

func TestEnsureFederationReplicaCredentialWriteFailureRetriesAdoption(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	project, err := store.CreateProjectWithUID(ctx, "spoke-project", replicaLocalProjectUID)
	require.NoError(t, err)
	_, _, err = store.CreateIssue(ctx, db.CreateIssueParams{
		ProjectID: project.ID,
		Title:     "local issue",
		Author:    "local-author",
	})
	require.NoError(t, err)
	params := replicaServiceParams()
	params.ProjectName = project.Name
	params.AdoptExisting = true
	injected := errors.New("injected adoption credential write failure")
	credentials.storeErr = injected
	wakes := 0

	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, params)

	require.ErrorIs(t, err, injected)
	adopted, projectErr := store.ProjectByID(ctx, project.ID)
	require.NoError(t, projectErr)
	assert.Equal(t, replicaHubProjectUID, adopted.UID)
	binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, bindErr)
	assert.True(t, binding.PushEnabled, "push is part of the atomic adoption state")
	_, ok, credentialErr := credentials.FederationCredential(ctx, adopted.UID)
	require.NoError(t, credentialErr)
	assert.False(t, ok)
	events, eventErr := store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, eventErr)
	require.Len(t, events, 1)
	assert.Zero(t, wakes)

	credentials.storeErr = nil
	result, err := daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, params)

	require.NoError(t, err)
	assert.False(t, result.Adopted)
	assert.Zero(t, result.AdoptionSnapshotCount)
	events, err = store.EventsAfter(ctx, db.EventsAfterParams{ProjectID: project.ID, Limit: 10})
	require.NoError(t, err)
	assert.Len(t, events, 1)
	_, ok, err = credentials.FederationCredential(ctx, adopted.UID)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, 1, wakes)
}

func TestEnsureFederationReplicaPushEnableFailureRetriesWithoutDuplication(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	_, err := store.ExecContext(ctx, `
		CREATE TRIGGER fail_federation_replica_push_enable
		BEFORE UPDATE OF push_enabled ON federation_bindings
		WHEN OLD.push_enabled = 0 AND NEW.push_enabled = 1
		BEGIN
			SELECT RAISE(FAIL, 'injected federation replica push failure');
		END`)
	require.NoError(t, err)
	wakes := 0

	_, err = daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, "injected federation replica push failure")
	project, projectErr := store.ProjectByUID(ctx, replicaHubProjectUID)
	require.NoError(t, projectErr)
	binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, bindErr)
	assert.False(t, binding.PushEnabled)
	stored, ok, credentialErr := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, credentialErr)
	require.True(t, ok)
	assert.Equal(t, "enrollment-token", stored.Token)
	assert.Zero(t, wakes)
	_, err = store.ExecContext(ctx, `DROP TRIGGER fail_federation_replica_push_enable`)
	require.NoError(t, err)

	result, err := daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.NoError(t, err)
	assert.True(t, result.Binding.PushEnabled)
	assert.Equal(t, 1, wakes)
	projects, err := store.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
}

func TestEnsureFederationReplicaRejectsBindingConflictsWithoutWaking(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*db.FederationBinding)
	}{
		{
			name: "different origin",
			mutate: func(binding *db.FederationBinding) {
				binding.HubURL = "https://other-hub.example"
			},
		},
		{
			name: "different hub project UID",
			mutate: func(binding *db.FederationBinding) {
				binding.HubProjectUID = replicaLocalProjectUID
			},
		},
		{
			name: "different role",
			mutate: func(binding *db.FederationBinding) {
				binding.Role = db.FederationRoleHub
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			store := openReplicaServiceStore(t)
			credentials := newReplicaCredentialStore()
			project, err := store.CreateProjectWithUID(ctx, "hub-project", replicaHubProjectUID)
			require.NoError(t, err)
			binding := db.FederationBinding{
				ProjectID:            project.ID,
				Role:                 db.FederationRoleSpoke,
				HubURL:               "http://hub.example",
				HubProjectID:         42,
				HubProjectUID:        replicaHubProjectUID,
				ReplayHorizonEventID: 9,
				PullCursorEventID:    8,
				Actor:                "sync-agent",
				Enabled:              true,
			}
			tt.mutate(&binding)
			_, err = store.UpsertFederationBinding(ctx, binding)
			require.NoError(t, err)
			params := replicaServiceParams()
			params.Credential.Token = ""
			wakes := 0

			_, err = daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, params)

			require.Error(t, err)
			assert.ErrorIs(t, err, daemon.ErrFederationReplicaBindingConflict)
			var serviceErr *daemon.FederationReplicaError
			assert.ErrorAs(t, err, &serviceErr)
			assert.Zero(t, wakes)
		})
	}
}

func TestEnsureFederationReplicaRejectsMalformedStoredBindingOrigin(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	project, err := store.CreateProjectWithUID(ctx, "hub-project", replicaHubProjectUID)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://hub.example",
		HubProjectID:         42,
		HubProjectUID:        replicaHubProjectUID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		Actor:                "sync-agent",
		Enabled:              true,
	})
	require.NoError(t, err)
	_, err = store.ExecContext(
		ctx,
		`UPDATE federation_bindings SET hub_url = 'not-an-http-origin' WHERE project_id = ?`,
		project.ID,
	)
	require.NoError(t, err)
	params := replicaServiceParams()
	params.Credential.Token = ""
	wakes := 0

	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, func() { wakes++ }, params)

	require.Error(t, err)
	assert.ErrorIs(t, err, daemon.ErrFederationReplicaBindingConflict)
	binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
	require.NoError(t, bindErr)
	assert.Equal(t, "not-an-http-origin", binding.HubURL)
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaDoesNotOverwriteConflictingCredential(t *testing.T) {
	ctx := context.Background()
	store := openReplicaServiceStore(t)
	credentials := newReplicaCredentialStore()
	project, err := store.CreateProjectWithUID(ctx, "hub-project", replicaHubProjectUID)
	require.NoError(t, err)
	_, err = store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               "http://hub.example",
		HubProjectID:         42,
		HubProjectUID:        replicaHubProjectUID,
		ReplayHorizonEventID: 9,
		PullCursorEventID:    8,
		Actor:                "sync-agent",
		Enabled:              true,
	})
	require.NoError(t, err)
	original := config.FederationCredential{
		HubURL:         "https://different-hub.example",
		HubProjectID:   73,
		Token:          "original-token",
		Capabilities:   "pull",
		Actor:          "manual-agent",
		HubProjectName: "other-project",
	}
	credentials.credentials[project.UID] = original
	wakes := 0

	_, err = daemon.EnsureFederationReplica(
		ctx, store, credentials, func() { wakes++ }, replicaServiceParams(),
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
	assert.Equal(t, original, credentials.credentials[project.UID])
	assert.Zero(t, credentials.storeCallCount())
	assert.Zero(t, wakes)
}

func TestEnsureFederationReplicaRejectsInvalidCanonicalOriginsBeforeMutation(t *testing.T) {
	t.Run("requested origin", func(t *testing.T) {
		ctx := context.Background()
		store := openReplicaServiceStore(t)
		params := replicaServiceParams()
		params.HubURL = "hub.example/no-scheme"
		params.Credential.HubURL = params.HubURL
		wakes := 0

		_, err := daemon.EnsureFederationReplica(
			ctx, store, newReplicaCredentialStore(), func() { wakes++ }, params,
		)

		require.Error(t, err)
		assert.ErrorIs(t, err, daemon.ErrFederationReplicaInvalidInput)
		projects, listErr := store.ListProjects(ctx)
		require.NoError(t, listErr)
		assert.Empty(t, projects)
		assert.Zero(t, wakes)
	})

	t.Run("stored credential origin", func(t *testing.T) {
		store := openReplicaServiceStore(t)
		credentials := newReplicaCredentialStore()
		original := config.FederationCredential{
			HubURL:       "not-an-http-origin",
			HubProjectID: 42,
			Token:        "original-token",
		}
		credentials.credentials[replicaHubProjectUID] = original

		_, err := daemon.EnsureFederationReplica(
			context.Background(), store, credentials, nil, replicaServiceParams(),
		)

		require.Error(t, err)
		assert.ErrorIs(t, err, daemon.ErrFederationReplicaCredentialConflict)
		assert.Equal(t, original, credentials.credentials[replicaHubProjectUID])
		assert.Zero(t, credentials.storeCallCount())
	})
}

func replicaServiceParams() daemon.EnsureFederationReplicaParams {
	return daemon.EnsureFederationReplicaParams{
		HubURL:                 "http://hub.example",
		HubProjectID:           42,
		HubProjectUID:          replicaHubProjectUID,
		ProjectName:            "hub-project",
		ReplayHorizonEventID:   9,
		BaselineThroughEventID: 12,
		Credential: config.FederationCredential{
			HubURL:         "http://hub.example",
			HubProjectID:   42,
			Token:          "enrollment-token",
			Capabilities:   " push, pull, push ",
			Actor:          "sync-agent",
			AllowInsecure:  false,
			HubProjectName: "",
		},
		PushEnabled: true,
	}
}

func openReplicaServiceStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	unsetReplicaAuthToken(t)
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func unsetReplicaAuthToken(t *testing.T) {
	t.Helper()
	value, existed := os.LookupEnv("KATA_AUTH_TOKEN")
	require.NoError(t, os.Unsetenv("KATA_AUTH_TOKEN"))
	t.Cleanup(func() {
		if existed {
			require.NoError(t, os.Setenv("KATA_AUTH_TOKEN", value))
			return
		}
		require.NoError(t, os.Unsetenv("KATA_AUTH_TOKEN"))
	})
}

var _ config.FederationCredentialStore = (*replicaCredentialStore)(nil)
