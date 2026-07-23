package federationconfig_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/db/sqlitestore"
	"go.kenn.io/kata/internal/federationconfig"
)

const (
	hubProjectUID       = "01HZNQ7VFPK1XGD8R5MABCD4EX"
	recreatedProjectUID = "01HZNQ7VFPK1XGD8R5MABCD4EY"
)

type fakeHub struct {
	mu                sync.Mutex
	project           federationconfig.HubProject
	enrollment        federationconfig.Enrollment
	resolveProjectErr error
	ensureProjectErr  error
	enrollmentErr     error
	rotationErr       error
	resolveCalls      int
	ensureCalls       int
	enrollmentCalls   []federationconfig.EnrollmentRequest
	rotationCalls     []federationconfig.EnrollmentRequest
	onEnsureProject   func()
	onEnrollment      func(federationconfig.EnrollmentRequest)
	onRotation        func(federationconfig.EnrollmentRequest)

	ensureEnrollmentStarted chan struct{}
	releaseEnsureEnrollment chan struct{}
	rotateEnrollmentStarted chan struct{}
	releaseRotateEnrollment chan struct{}
}

func (h *fakeHub) ResolveProject(
	_ context.Context, name string,
) (federationconfig.HubProject, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolveCalls++
	if h.resolveProjectErr != nil {
		return federationconfig.HubProject{}, h.resolveProjectErr
	}
	project := h.project
	project.Name = name
	project.ReplayHorizonEventID = 0
	project.BaselineThroughEventID = 0
	return project, nil
}

func (h *fakeHub) EnsureProject(
	_ context.Context, name, actor string,
) (federationconfig.HubProject, error) {
	h.mu.Lock()
	h.ensureCalls++
	onEnsureProject := h.onEnsureProject
	if h.ensureProjectErr != nil {
		h.mu.Unlock()
		return federationconfig.HubProject{}, h.ensureProjectErr
	}
	project := h.project
	h.mu.Unlock()
	if onEnsureProject != nil {
		onEnsureProject()
	}
	project.Name = name
	_ = actor
	return project, nil
}

func (h *fakeHub) EnsureEnrollment(
	ctx context.Context, request federationconfig.EnrollmentRequest,
) (federationconfig.Enrollment, error) {
	h.mu.Lock()
	h.enrollmentCalls = append(h.enrollmentCalls, request)
	if h.ensureEnrollmentStarted != nil {
		close(h.ensureEnrollmentStarted)
	}
	if h.releaseEnsureEnrollment != nil {
		select {
		case <-h.releaseEnsureEnrollment:
		case <-ctx.Done():
			h.mu.Unlock()
			return federationconfig.Enrollment{}, ctx.Err()
		}
	}
	onEnrollment := h.onEnrollment
	enrollment := h.enrollment
	err := h.enrollmentErr
	h.mu.Unlock()
	if onEnrollment != nil {
		onEnrollment(request)
	}
	return enrollment, err
}

func (h *fakeHub) RotateEnrollment(
	ctx context.Context, request federationconfig.EnrollmentRequest,
) (federationconfig.Enrollment, error) {
	h.mu.Lock()
	h.rotationCalls = append(h.rotationCalls, request)
	if h.rotateEnrollmentStarted != nil {
		close(h.rotateEnrollmentStarted)
	}
	if h.releaseRotateEnrollment != nil {
		select {
		case <-h.releaseRotateEnrollment:
		case <-ctx.Done():
			h.mu.Unlock()
			return federationconfig.Enrollment{}, ctx.Err()
		}
	}
	onRotation := h.onRotation
	enrollment := h.enrollment
	err := h.rotationErr
	h.mu.Unlock()
	if onRotation != nil {
		onRotation(request)
	}
	return enrollment, err
}

type fakeCredentialStore struct {
	mu                  sync.Mutex
	credentials         map[string]config.FederationCredential
	storeCalls          []credentialStoreCall
	managedReserveCalls []config.FederationManagedCredentialReservation
	managedDeleteCalls  []config.FederationManagedCredentialReservation
	deleteCalls         []string
	rekeyCalls          []config.FederationCredentialRekey
	readErr             error
	storeErr            error
	rekeyErr            error
	failStoreAt         int
}

type credentialStoreCall struct {
	projectUID string
	credential config.FederationCredential
}

type baseCredentialStore struct {
	delegate *fakeCredentialStore
}

var _ config.FederationManagedCredentialStore = (*fakeCredentialStore)(nil)
var _ config.FederationCredentialStore = (*baseCredentialStore)(nil)

func (s baseCredentialStore) FederationCredential(
	ctx context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	return s.delegate.FederationCredential(ctx, projectUID)
}

func (s baseCredentialStore) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	return s.delegate.StoreFederationCredential(ctx, projectUID, credential)
}

func (s baseCredentialStore) DeleteFederationCredential(
	ctx context.Context, projectUID string,
) error {
	return s.delegate.DeleteFederationCredential(ctx, projectUID)
}

type failProjectByUIDStore struct {
	db.Storage
	mu      sync.Mutex
	failUID string
	failAt  int
	calls   int
}

type replicaCallKindContextKey struct{}

type orderedReplicaServiceStore struct {
	db.Storage
	hubProjectUID string
	mu            sync.Mutex
	seen          map[string]bool
	arrived       map[string]chan struct{}
	release       map[string]chan struct{}
}

func newOrderedReplicaServiceStore(
	store db.Storage, hubUID string, kinds ...string,
) *orderedReplicaServiceStore {
	arrived := make(map[string]chan struct{}, len(kinds))
	release := make(map[string]chan struct{}, len(kinds))
	for _, kind := range kinds {
		arrived[kind] = make(chan struct{})
		release[kind] = make(chan struct{})
	}
	return &orderedReplicaServiceStore{
		Storage: store, hubProjectUID: hubUID,
		seen: make(map[string]bool), arrived: arrived, release: release,
	}
}

func (s *orderedReplicaServiceStore) ProjectByUID(
	ctx context.Context, projectUID string,
) (db.Project, error) {
	kind, _ := ctx.Value(replicaCallKindContextKey{}).(string)
	if projectUID == s.hubProjectUID && kind != "" {
		s.mu.Lock()
		first := !s.seen[kind]
		s.seen[kind] = true
		arrived := s.arrived[kind]
		release := s.release[kind]
		s.mu.Unlock()
		if first {
			close(arrived)
			select {
			case <-release:
			case <-ctx.Done():
				return db.Project{}, ctx.Err()
			}
		}
	}
	return s.Storage.ProjectByUID(ctx, projectUID)
}

func (s *failProjectByUIDStore) ProjectByUID(
	ctx context.Context, projectUID string,
) (db.Project, error) {
	s.mu.Lock()
	if projectUID == s.failUID {
		s.calls++
	}
	if projectUID == s.failUID && s.calls == s.failAt {
		s.mu.Unlock()
		return db.Project{}, errors.New("injected project lookup failure")
	}
	s.mu.Unlock()
	return s.Storage.ProjectByUID(ctx, projectUID)
}

func newFakeCredentialStore() *fakeCredentialStore {
	return &fakeCredentialStore{credentials: make(map[string]config.FederationCredential)}
}

func (s *fakeCredentialStore) FederationCredential(
	_ context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return config.FederationCredential{}, false, s.readErr
	}
	credential, ok := s.credentials[projectUID]
	return credential, ok, nil
}

func (s *fakeCredentialStore) StoreFederationCredential(
	_ context.Context, projectUID string, credential config.FederationCredential,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storeCalls = append(s.storeCalls, credentialStoreCall{projectUID: projectUID, credential: credential})
	if s.storeErr != nil && (s.failStoreAt == 0 || len(s.storeCalls) == s.failStoreAt) {
		return s.storeErr
	}
	s.credentials[projectUID] = credential
	return nil
}

func (s *fakeCredentialStore) DeleteFederationCredential(
	_ context.Context, projectUID string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls = append(s.deleteCalls, projectUID)
	delete(s.credentials, projectUID)
	return nil
}

func (s *fakeCredentialStore) RekeyFederationCredential(
	_ context.Context, rekey config.FederationCredentialRekey,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rekeyCalls = append(s.rekeyCalls, rekey)
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

func (s *fakeCredentialStore) ReserveManagedFederationCredential(
	_ context.Context, reservation config.FederationManagedCredentialReservation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managedReserveCalls = append(s.managedReserveCalls, reservation)
	if existing, found := s.credentials[reservation.ProjectUID]; found &&
		existing != reservation.Credential {
		return config.ErrFederationCredentialConflict
	}
	s.credentials[reservation.ProjectUID] = reservation.Credential
	return nil
}

func (s *fakeCredentialStore) FindManagedFederationCredential(
	_ context.Context, projectName string,
) (config.FederationManagedCredentialReservation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var match config.FederationManagedCredentialReservation
	found := false
	for projectUID, credential := range s.credentials {
		if !credential.ManagedByConfig || credential.SpokeProjectName != projectName {
			continue
		}
		if found {
			return config.FederationManagedCredentialReservation{}, false,
				config.ErrFederationCredentialConflict
		}
		match = config.FederationManagedCredentialReservation{
			ProjectUID: projectUID,
			Credential: credential,
		}
		found = true
	}
	return match, found, nil
}

func (s *fakeCredentialStore) DeleteManagedFederationCredential(
	_ context.Context, reservation config.FederationManagedCredentialReservation,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.managedDeleteCalls = append(s.managedDeleteCalls, reservation)
	current, found := s.credentials[reservation.ProjectUID]
	if !found {
		return nil
	}
	if current != reservation.Credential || !current.ManagedByConfig {
		return config.ErrFederationCredentialConflict
	}
	delete(s.credentials, reservation.ProjectUID)
	return nil
}

func (s *fakeCredentialStore) get(projectUID string) (config.FederationCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credential, ok := s.credentials[projectUID]
	return credential, ok
}

func TestReconcileMappingCreatesEnrollsAdoptsPushesAndWakes(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	wakes := 0
	hub.onEnrollment = func(request federationconfig.EnrollmentRequest) {
		project, err := store.ProjectByName(context.Background(), "spoke-project")
		require.NoError(t, err, "local project must exist before hub enrollment")
		assert.NotEqual(t, hubProjectUID, project.UID)
		_, ok := credentials.get(project.UID)
		assert.False(t, ok, "generated reservation must not use the local UID")
		persisted, ok := credentials.get(hubProjectUID)
		require.True(t, ok, "enrollment token must be durable before hub enrollment")
		assert.Equal(t, persisted.Token, request.Token)
		assert.NotEmpty(t, request.Token)
	}

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)

	project, err := store.ProjectByName(context.Background(), "spoke-project")
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, project.UID)
	binding, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, db.FederationRoleSpoke, binding.Role)
	assert.Equal(t, hubProjectUID, binding.HubProjectUID)
	assert.True(t, binding.Enabled)
	assert.True(t, binding.PushEnabled)
	assert.Equal(t, 1, wakes)

	require.Len(t, hub.enrollmentCalls, 1)
	request := hub.enrollmentCalls[0]
	assert.Equal(t, store.InstanceUID(), request.SpokeInstanceUID)
	assert.Equal(t, int64(42), request.ProjectID)
	assert.Equal(t, "claim,pull,push", request.Capabilities)
	assert.Equal(t, "user-a", request.Actor)
	assert.True(t, request.AllowAdoptionSnapshotAuthors)
	assert.Empty(t, hub.rotationCalls)

	credential, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, request.Token, credential.Token)
	assert.Equal(t, "identity-user", credential.Actor)
	assert.Equal(t, "claim,pull,push", credential.Capabilities)
	assert.True(t, credential.ManagedByConfig)
	assert.Equal(t, "primary", credential.HubCatalog)
	assert.Equal(t, "hub-project", credential.HubProjectName)
	assert.Equal(t, "user-a", credential.RequestedActor)
	assert.Equal(t, "spoke-project", credential.SpokeProjectName)
}

func TestReconcileMappingInitialReservationUsesOnlyHubUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	enrollmentArrived := make(chan struct{})
	releaseEnrollment := make(chan struct{})
	hub.onEnrollment = func(federationconfig.EnrollmentRequest) {
		close(enrollmentArrived)
		select {
		case <-releaseEnrollment:
		case <-ctx.Done():
		}
	}

	result := make(chan error, 1)
	go func() {
		result <- federationconfig.ReconcileMapping(
			ctx, store, credentials, hub, testCatalog(), testMapping(), nil,
		)
	}()

	select {
	case <-enrollmentArrived:
	case <-ctx.Done():
		require.FailNow(t, "wait for enrollment", "error: %v", ctx.Err())
	}
	localProject, err := store.ProjectByName(ctx, "spoke-project")
	require.NoError(t, err)
	_, localFound, err := credentials.FederationCredential(ctx, localProject.UID)
	require.NoError(t, err)
	hubCredential, hubFound, err := credentials.FederationCredential(ctx, hubProjectUID)
	require.NoError(t, err)
	assert.False(t, localFound)
	assert.True(t, hubFound)
	assert.True(t, hubCredential.ManagedByConfig)
	assert.Equal(t, "spoke-project", hubCredential.SpokeProjectName)

	close(releaseEnrollment)
	select {
	case err = <-result:
	case <-ctx.Done():
		require.FailNow(t, "wait for reconciliation", "error: %v", ctx.Err())
	}
	require.NoError(t, err)
}

func TestReconcileMappingGeneratedReservationRequiresManagedStore(t *testing.T) {
	ctx := t.Context()
	store := openReconcileStore(t)
	delegate := newFakeCredentialStore()
	credentials := &baseCredentialStore{delegate: delegate}
	hub := newFakeHub()

	err := federationconfig.ReconcileMapping(
		ctx, store, credentials, hub, testCatalog(), testMapping(), nil,
	)

	require.ErrorIs(t, err, federationconfig.ErrCredentialIO)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Zero(t, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	projects, listErr := store.ListProjects(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, projects)
	assert.Empty(t, delegate.credentials)
}

func TestReconcileMappingManagedCleanupLookupRequiresManagedStore(t *testing.T) {
	ctx := t.Context()
	store := openReconcileStore(t)
	delegate := newFakeCredentialStore()
	pending := managedCredential()
	pending.Actor = ""
	delegate.credentials[hubProjectUID] = pending
	credentials := &baseCredentialStore{delegate: delegate}
	hub := newFakeHub()

	err := federationconfig.ReconcileMapping(
		ctx, store, credentials, hub, testCatalog(), testMapping(), nil,
	)

	require.ErrorIs(t, err, federationconfig.ErrCredentialIO)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Zero(t, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	projects, listErr := store.ListProjects(ctx)
	require.NoError(t, listErr)
	assert.Empty(t, projects)
	final, found := delegate.get(hubProjectUID)
	require.True(t, found)
	assert.Equal(t, pending, final)
}

func TestReconcileMappingManualCredentialMovesLocalUIDToHubUID(t *testing.T) {
	ctx := t.Context()
	store := openReconcileStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubCatalog = ""
	manual.HubProjectName = ""
	manual.RequestedActor = ""
	manual.SpokeProjectName = ""
	credentials.credentials[project.UID] = manual

	err = federationconfig.ReconcileMapping(
		ctx, store, credentials, newFakeHub(), testCatalog(), testMapping(), nil,
	)
	require.NoError(t, err)

	_, localFound, err := credentials.FederationCredential(ctx, project.UID)
	require.NoError(t, err)
	hubCredential, hubFound, err := credentials.FederationCredential(ctx, hubProjectUID)
	require.NoError(t, err)
	assert.False(t, localFound)
	require.True(t, hubFound)
	assert.Equal(t, manual.Token, hubCredential.Token)
	assert.True(t, hubCredential.ManagedByConfig)
	assert.Equal(t, "spoke-project", hubCredential.SpokeProjectName)
}

func TestReconcileMappingAdoptsExistingStandaloneWorkWithSnapshot(t *testing.T) {
	store := openReconcileStore(t)
	project, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	_, _, err = store.CreateIssue(context.Background(), db.CreateIssueParams{
		ProjectID: project.ID, Title: "local issue", Author: "local-author",
	})
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	wakes := 0

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)

	bound, err := store.ProjectByName(context.Background(), "spoke-project")
	require.NoError(t, err)
	assert.Equal(t, project.ID, bound.ID)
	assert.Equal(t, hubProjectUID, bound.UID)
	events, err := store.EventsAfter(context.Background(), db.EventsAfterParams{
		ProjectID: bound.ID, Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "issue.snapshot", events[0].Type)
	assert.Equal(t, "identity-user", events[0].Actor)
	assert.Equal(t, 1, wakes)
}

func TestReconcileMappingAtomicRekeyFailureLeavesStandaloneStateRetryable(t *testing.T) {
	store := openReconcileStore(t)
	project, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubCatalog = ""
	manual.HubProjectName = ""
	manual.RequestedActor = ""
	credentials.credentials[project.UID] = manual
	credentials.rekeyErr = errors.New("injected atomic rekey failure")
	hub := newFakeHub()
	wakes := 0

	firstErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.Error(t, firstErr)
	assert.ErrorIs(t, firstErr, federationconfig.ErrCredentialIO)
	require.Len(t, hub.enrollmentCalls, 1)
	assert.Equal(t, manual.Token, hub.enrollmentCalls[0].Token)
	require.Len(t, credentials.rekeyCalls, 1)
	assert.Equal(t, project.UID, credentials.rekeyCalls[0].FromProjectUID)
	assert.Equal(t, hubProjectUID, credentials.rekeyCalls[0].ToProjectUID)
	assert.Equal(t, manual, credentials.rekeyCalls[0].Expected)
	assert.Equal(t, "identity-user", credentials.rekeyCalls[0].Replacement.Actor)
	assert.True(t, credentials.rekeyCalls[0].Replacement.ManagedByConfig)
	assert.Empty(t, credentials.storeCalls)
	assert.Empty(t, credentials.deleteCalls)
	assert.Zero(t, wakes)

	unchanged, err := store.ProjectByName(context.Background(), project.Name)
	require.NoError(t, err)
	assert.Equal(t, project, unchanged)
	_, bindingErr := store.FederationBindingByProject(context.Background(), project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	localCredential, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, manual, localCredential)
	_, ok = credentials.get(hubProjectUID)
	assert.False(t, ok)

	credentials.rekeyErr = nil
	secondErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, secondErr)
	require.Len(t, hub.enrollmentCalls, 2)
	assert.Equal(t, hub.enrollmentCalls[0], hub.enrollmentCalls[1])
	require.Len(t, credentials.rekeyCalls, 2)
	assert.Empty(t, hub.rotationCalls)
	assert.Equal(t, 1, wakes)

	adopted, err := store.ProjectByName(context.Background(), project.Name)
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, adopted.UID)
	binding, err := store.FederationBindingByProject(context.Background(), adopted.ID)
	require.NoError(t, err)
	assert.True(t, binding.Enabled)
	assert.True(t, binding.PushEnabled)
	final, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, manual.Token, final.Token)
	assert.Equal(t, "identity-user", final.Actor)
	assert.True(t, final.ManagedByConfig)
	_, ok = credentials.get(project.UID)
	assert.False(t, ok)
	assert.Empty(t, credentials.deleteCalls)
}

func TestReconcileMappingAtomicRekeyCrashBeforeAdoptionReusesHubKeyOnRestart(t *testing.T) {
	baseStore := openReconcileStore(t)
	project, err := baseStore.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	store := &failProjectByUIDStore{
		Storage: baseStore, failUID: hubProjectUID, failAt: 3,
	}
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubCatalog = ""
	manual.HubProjectName = ""
	manual.RequestedActor = ""
	credentials.credentials[project.UID] = manual
	hub := newFakeHub()
	wakes := 0

	firstErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.Error(t, firstErr)
	assert.ErrorIs(t, firstErr, federationconfig.ErrLocalStorage)
	require.Len(t, hub.enrollmentCalls, 1)
	assert.Equal(t, manual.Token, hub.enrollmentCalls[0].Token)
	require.Len(t, credentials.rekeyCalls, 1)
	assert.Empty(t, credentials.storeCalls)
	assert.Empty(t, credentials.deleteCalls)
	assert.Zero(t, wakes)

	unchanged, err := baseStore.ProjectByName(context.Background(), project.Name)
	require.NoError(t, err)
	assert.Equal(t, project, unchanged)
	_, bindingErr := baseStore.FederationBindingByProject(context.Background(), project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
	_, ok := credentials.get(project.UID)
	assert.False(t, ok)
	rekeyed, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, manual.Token, rekeyed.Token)
	assert.Equal(t, "identity-user", rekeyed.Actor)
	assert.True(t, rekeyed.ManagedByConfig)

	secondErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, secondErr)
	require.Len(t, hub.enrollmentCalls, 2)
	assert.Equal(t, hub.enrollmentCalls[0], hub.enrollmentCalls[1])
	assert.Empty(t, hub.rotationCalls)
	require.Len(t, credentials.rekeyCalls, 1)
	assert.Equal(t, 1, wakes)

	adopted, err := baseStore.ProjectByName(context.Background(), project.Name)
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, adopted.UID)
	final, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, manual.Token, final.Token)
	assert.Equal(t, "identity-user", final.Actor)
	_, ok = credentials.get(project.UID)
	assert.False(t, ok)
	assert.Empty(t, credentials.deleteCalls)
}

func TestReconcileMappingServicePrevalidationFailureDoesNotRekeyOutsideLock(t *testing.T) {
	baseStore := openReconcileStore(t)
	project, err := baseStore.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	store := &failProjectByUIDStore{
		Storage: baseStore, failUID: hubProjectUID, failAt: 1,
	}
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubCatalog = ""
	manual.HubProjectName = ""
	manual.RequestedActor = ""
	credentials.credentials[project.UID] = manual
	hub := newFakeHub()

	reconcileErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)

	require.ErrorIs(t, reconcileErr, federationconfig.ErrLocalStorage)
	assert.Empty(t, credentials.rekeyCalls)
	localCredential, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, manual, localCredential)
	_, ok = credentials.get(hubProjectUID)
	assert.False(t, ok)
	unchanged, err := baseStore.ProjectByName(context.Background(), project.Name)
	require.NoError(t, err)
	assert.Equal(t, project.UID, unchanged.UID)
}

func TestReconcileMappingRacesManualJoinWithoutOrphanOrManagedOverwrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	baseStore := openReconcileStore(t)
	project, err := baseStore.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	const (
		configCall = "config"
		manualCall = "manual"
	)
	store := newOrderedReplicaServiceStore(baseStore, hubProjectUID, configCall, manualCall)
	credentials := newFakeCredentialStore()
	configCredential := managedCredential()
	configCredential.ManagedByConfig = false
	configCredential.HubCatalog = ""
	configCredential.HubProjectName = ""
	configCredential.RequestedActor = ""
	credentials.credentials[project.UID] = configCredential
	hub := newFakeHub()

	configResult := make(chan error, 1)
	go func() {
		configResult <- federationconfig.ReconcileMapping(
			context.WithValue(ctx, replicaCallKindContextKey{}, configCall),
			store, credentials, hub, testCatalog(), testMapping(), nil,
		)
	}()

	manualParams := daemon.EnsureFederationReplicaParams{
		HubURL:               "https://hub.example",
		HubProjectID:         42,
		HubProjectUID:        hubProjectUID,
		ProjectName:          project.Name,
		ReplayHorizonEventID: 9,
		Credential: config.FederationCredential{
			HubURL: "https://hub.example", HubProjectID: 42,
			Token: "manual-token-b", Capabilities: "claim,pull,push",
			Actor: "identity-user",
		},
		PushEnabled: true, AdoptExisting: true,
	}
	manualResult := make(chan error, 1)
	go func() {
		_, callErr := daemon.EnsureFederationReplica(
			context.WithValue(ctx, replicaCallKindContextKey{}, manualCall),
			store, credentials, nil, manualParams,
		)
		manualResult <- callErr
	}()

	select {
	case <-store.arrived[configCall]:
	case <-ctx.Done():
		require.FailNow(t, "wait for config service prevalidation", "error: %v", ctx.Err())
	}
	select {
	case <-store.arrived[manualCall]:
	case <-ctx.Done():
		require.FailNow(t, "wait for manual service prevalidation", "error: %v", ctx.Err())
	}

	close(store.release[manualCall])
	var manualErr error
	select {
	case manualErr = <-manualResult:
	case <-ctx.Done():
		require.FailNow(t, "wait for manual join result", "error: %v", ctx.Err())
	}
	require.ErrorIs(t, manualErr, daemon.ErrFederationReplicaCredentialConflict)

	close(store.release[configCall])
	select {
	case err = <-configResult:
	case <-ctx.Done():
		require.FailNow(t, "wait for config reconciliation result", "error: %v", ctx.Err())
	}
	require.NoError(t, err)

	adopted, err := baseStore.ProjectByName(ctx, project.Name)
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, adopted.UID)
	_, ok := credentials.get(project.UID)
	assert.False(t, ok, "serialized adoption must not leave an obsolete L credential")
	final, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, configCredential.Token, final.Token)
	assert.True(t, final.ManagedByConfig)

	_, retryErr := daemon.EnsureFederationReplica(ctx, store, credentials, nil, manualParams)
	require.ErrorIs(t, retryErr, daemon.ErrFederationReplicaCredentialConflict)
	afterRetry, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, final, afterRetry, "manual retry must not overwrite the managed credential")
}

func TestReconcileMappingManualH2JoinWinsBeforeH1ReservationWithoutEnrollment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	baseStore := openReconcileStore(t)
	project, err := baseStore.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	const manualCall = "manual-h2"
	store := newOrderedReplicaServiceStore(
		baseStore, recreatedProjectUID, manualCall,
	)
	credentials := newFakeCredentialStore()
	hubProjectEnsured := make(chan struct{})
	var ensureOnce sync.Once
	hub := newFakeHub()
	hub.onEnsureProject = func() {
		ensureOnce.Do(func() { close(hubProjectEnsured) })
	}

	manualParams := daemon.EnsureFederationReplicaParams{
		HubURL:               "https://hub.example",
		HubProjectID:         43,
		HubProjectUID:        recreatedProjectUID,
		ProjectName:          project.Name,
		ReplayHorizonEventID: 9,
		Credential: config.FederationCredential{
			HubURL: "https://hub.example", HubProjectID: 43,
			Token: "manual-token-b", Capabilities: "claim,pull,push",
			Actor: "identity-user",
		},
		PushEnabled: true, AdoptExisting: true,
	}
	manualResult := make(chan error, 1)
	go func() {
		_, callErr := daemon.EnsureFederationReplica(
			context.WithValue(ctx, replicaCallKindContextKey{}, manualCall),
			store, credentials, nil, manualParams,
		)
		manualResult <- callErr
	}()
	select {
	case <-store.arrived[manualCall]:
	case <-ctx.Done():
		require.FailNow(t, "wait for paused manual H2 join", "error: %v", ctx.Err())
	}

	configResult := make(chan error, 1)
	go func() {
		configResult <- federationconfig.ReconcileMapping(
			ctx, store, credentials, hub, testCatalog(), testMapping(), nil,
		)
	}()
	select {
	case <-hubProjectEnsured:
	case <-ctx.Done():
		require.FailNow(t, "wait for config hub project ensure", "error: %v", ctx.Err())
	}
	close(store.release[manualCall])

	select {
	case err = <-manualResult:
	case <-ctx.Done():
		require.FailNow(t, "wait for manual H2 join", "error: %v", ctx.Err())
	}
	require.NoError(t, err)
	select {
	case err = <-configResult:
	case <-ctx.Done():
		require.FailNow(t, "wait for losing H1 reconciliation", "error: %v", ctx.Err())
	}
	require.ErrorIs(t, err, federationconfig.ErrBindingConflict)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)

	adopted, err := baseStore.ProjectByName(ctx, project.Name)
	require.NoError(t, err)
	assert.Equal(t, recreatedProjectUID, adopted.UID)
	_, found := credentials.get(project.UID)
	assert.False(t, found)
	_, found = credentials.get(hubProjectUID)
	assert.False(t, found, "losing H1 reconciliation must not reserve a credential")
	winner, found := credentials.get(recreatedProjectUID)
	require.True(t, found)
	assert.Equal(t, "manual-token-b", winner.Token)
	assert.False(t, winner.ManagedByConfig)
}

func TestReconcileMappingH1ReservationWinsBeforeManualH2JoinAndHubEnrollment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := openReconcileStore(t)
	project, err := store.CreateProject(ctx, "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	enrollmentArrived := make(chan struct{})
	releaseEnrollment := make(chan struct{})
	hub := newFakeHub()
	hub.onEnrollment = func(federationconfig.EnrollmentRequest) {
		close(enrollmentArrived)
		select {
		case <-releaseEnrollment:
		case <-ctx.Done():
		}
	}
	configResult := make(chan error, 1)
	go func() {
		configResult <- federationconfig.ReconcileMapping(
			ctx, store, credentials, hub, testCatalog(), testMapping(), nil,
		)
	}()
	select {
	case <-enrollmentArrived:
	case <-ctx.Done():
		require.FailNow(t, "wait for enrollment after config reservation", "error: %v", ctx.Err())
	}

	reserved, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.True(t, reserved.ManagedByConfig)
	manualParams := daemon.EnsureFederationReplicaParams{
		HubURL:               "https://hub.example",
		HubProjectID:         43,
		HubProjectUID:        recreatedProjectUID,
		ProjectName:          project.Name,
		ReplayHorizonEventID: 9,
		Credential: config.FederationCredential{
			HubURL: "https://hub.example", HubProjectID: 43,
			Token: "manual-token-b", Capabilities: "claim,pull,push",
			Actor: "identity-user",
		},
		PushEnabled: true, AdoptExisting: true,
	}
	_, manualErr := daemon.EnsureFederationReplica(
		ctx, store, credentials, nil, manualParams,
	)
	require.ErrorIs(t, manualErr, daemon.ErrFederationReplicaCredentialConflict)
	stillReserved, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, reserved, stillReserved)
	unchanged, projectErr := store.ProjectByName(ctx, project.Name)
	require.NoError(t, projectErr)
	assert.Equal(t, project.UID, unchanged.UID)

	close(releaseEnrollment)
	select {
	case err = <-configResult:
	case <-ctx.Done():
		require.FailNow(t, "wait for config adoption", "error: %v", ctx.Err())
	}
	require.NoError(t, err)
	require.Len(t, hub.enrollmentCalls, 1)
	assert.Equal(t, reserved.Token, hub.enrollmentCalls[0].Token)
	adopted, err := store.ProjectByName(ctx, project.Name)
	require.NoError(t, err)
	assert.Equal(t, hubProjectUID, adopted.UID)
	final, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, reserved.Token, final.Token)
	assert.True(t, final.ManagedByConfig)
	_, ok = credentials.get(project.UID)
	assert.False(t, ok, "generated credentials must never use the local UID")
	_, ok = credentials.get(recreatedProjectUID)
	assert.False(t, ok, "losing manual H2 join must not write an H2 credential")
}

func TestReconcileMappingHubReturnedActorWinsCredentialAndBinding(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.enrollment.Actor = "token-bound-user"

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.NoError(t, err)

	project, err := store.ProjectByName(context.Background(), "spoke-project")
	require.NoError(t, err)
	credential, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, "token-bound-user", credential.Actor)
	assert.Equal(t, "user-a", credential.RequestedActor)
	binding, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "token-bound-user", binding.Actor)
}

func TestReconcileMappingRejectsMalformedHubProjectUIDBeforeFederationMutation(t *testing.T) {
	const malformedUID = "not-a-valid-hub-project-uid"
	store := openReconcileStore(t)
	project, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.project.UID = malformedUID
	wakes := 0

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrHubValidation)
	assert.NotContains(t, err.Error(), malformedUID)
	assert.NotContains(t, err.Error(), testCatalogBearer)
	assert.Empty(t, credentials.storeCalls)
	assert.Empty(t, credentials.deleteCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Zero(t, wakes)

	unchanged, readErr := store.ProjectByName(context.Background(), project.Name)
	require.NoError(t, readErr)
	assert.Equal(t, project, unchanged)
	_, bindingErr := store.FederationBindingByProject(context.Background(), project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestReconcileMappingRejectsInvalidEnrollmentActorWithoutStateOverwrite(t *testing.T) {
	tests := []struct {
		name  string
		actor string
	}{
		{name: "empty", actor: "   "},
		{name: "reserved", actor: "BOOTSTRAP"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project := createMatchingBoundProject(t, store)
			binding, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			binding.PushEnabled = false
			_, err = store.UpsertFederationBinding(context.Background(), binding)
			require.NoError(t, err)
			beforeBinding, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)

			credentials := newFakeCredentialStore()
			original := managedCredential()
			original.Actor = ""
			credentials.credentials[project.UID] = original
			hub := newFakeHub()
			hub.enrollment.Actor = tt.actor
			wakes := 0

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), func() { wakes++ },
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, federationconfig.ErrHubValidation)
			assert.NotContains(t, err.Error(), testCatalogBearer)
			assert.NotContains(t, err.Error(), original.Token)
			assert.NotContains(t, err.Error(), tt.actor)
			assert.Empty(t, credentials.storeCalls)
			assert.Empty(t, credentials.deleteCalls)
			require.Len(t, hub.rotationCalls, 1)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Zero(t, wakes)

			afterBinding, readErr := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, readErr)
			assert.Equal(t, beforeBinding, afterBinding)
			final, ok := credentials.get(project.UID)
			require.True(t, ok)
			assert.Equal(t, original, final)
		})
	}
}

func TestReconcileMappingCrashAfterCredentialPersistenceReusesToken(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.enrollmentErr = errors.New("hub unavailable")

	firstErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, firstErr)
	require.Len(t, hub.enrollmentCalls, 1)
	firstToken := hub.enrollmentCalls[0].Token
	assert.NotEmpty(t, firstToken)
	project, err := store.ProjectByName(context.Background(), "spoke-project")
	require.NoError(t, err)
	_, ok := credentials.get(project.UID)
	assert.False(t, ok)
	persisted, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, firstToken, persisted.Token)

	hub.enrollmentErr = nil
	secondErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.NoError(t, secondErr)
	require.Len(t, hub.enrollmentCalls, 2)
	assert.Equal(t, firstToken, hub.enrollmentCalls[1].Token)
	_, ok = credentials.get(project.UID)
	assert.False(t, ok, "generated credentials must never use the local UID")
	final, ok := credentials.get(hubProjectUID)
	require.True(t, ok)
	assert.Equal(t, firstToken, final.Token)
}

func TestReconcileMappingLostEnrollmentResponseRepeatsExactToken(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.enrollmentErr = errors.New("response lost after enrollment commit")

	firstErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, firstErr)
	require.Len(t, hub.enrollmentCalls, 1)
	first := hub.enrollmentCalls[0]

	hub.enrollmentErr = nil
	secondErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.NoError(t, secondErr)
	require.Len(t, hub.enrollmentCalls, 2)
	assert.Equal(t, first, hub.enrollmentCalls[1])
}

func TestReconcileMappingLeaveDuringEnrollmentDoesNotResurrectCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.ensureEnrollmentStarted = make(chan struct{})
	hub.releaseEnsureEnrollment = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- federationconfig.ReconcileMapping(
			ctx, store, credentials, hub,
			testCatalog(), testMapping(), nil,
		)
	}()

	select {
	case <-hub.ensureEnrollmentStarted:
	case <-ctx.Done():
		require.FailNow(t, "wait for paused enrollment", "error: %v", ctx.Err())
	}
	localProject, err := store.ProjectByName(ctx, "spoke-project")
	require.NoError(t, err)
	_, err = daemon.LeaveFederationReplica(
		ctx, store, credentials, nil, localProject.ID,
	)
	require.NoError(t, err)
	close(hub.releaseEnsureEnrollment)

	select {
	case err = <-result:
	case <-ctx.Done():
		require.FailNow(t, "wait for reconciliation after leave", "error: %v", ctx.Err())
	}
	require.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	_, found, readErr := credentials.FederationCredential(ctx, hubProjectUID)
	require.NoError(t, readErr)
	assert.False(t, found)
	_, bindingErr := store.FederationBindingByProject(ctx, localProject.ID)
	require.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestReconcileMappingLeaveDuringRotationDoesNotResurrectCredential(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	store := openReconcileStore(t)
	localProject := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.rotateEnrollmentStarted = make(chan struct{})
	hub.releaseRotateEnrollment = make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- federationconfig.ReconcileMapping(
			ctx, store, credentials, hub,
			testCatalog(), testMapping(), nil,
		)
	}()

	select {
	case <-hub.rotateEnrollmentStarted:
	case <-ctx.Done():
		require.FailNow(t, "wait for paused rotation", "error: %v", ctx.Err())
	}
	_, err := daemon.LeaveFederationReplica(
		ctx, store, credentials, nil, localProject.ID,
	)
	require.NoError(t, err)
	close(hub.releaseRotateEnrollment)

	select {
	case err = <-result:
	case <-ctx.Done():
		require.FailNow(t, "wait for reconciliation after leave", "error: %v", ctx.Err())
	}
	require.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	_, found, readErr := credentials.FederationCredential(ctx, hubProjectUID)
	require.NoError(t, readErr)
	assert.False(t, found)
	_, bindingErr := store.FederationBindingByProject(ctx, localProject.ID)
	require.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestReconcileMappingMissingCredentialRotatesEnrollment(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	wakes := 0
	hub.onRotation = func(request federationconfig.EnrollmentRequest) {
		persisted, ok := credentials.get(project.UID)
		require.True(t, ok, "replacement token must be durable before rotation")
		assert.Equal(t, persisted.Token, request.Token)
		assert.NotEmpty(t, request.Token)
	}

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)
	assert.Empty(t, hub.enrollmentCalls)
	require.Len(t, hub.rotationCalls, 1)
	assert.Equal(t, store.InstanceUID(), hub.rotationCalls[0].SpokeInstanceUID)
	credential, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, hub.rotationCalls[0].Token, credential.Token)
	assert.True(t, credential.ManagedByConfig)
	assert.Equal(t, "identity-user", credential.Actor)
	assert.Equal(t, 1, wakes)
}

func TestReconcileMappingIncompleteReplacementReplaysRotation(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()
	hub.rotationErr = errors.New("response lost after rotation commit")

	firstErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, firstErr)
	assert.ErrorIs(t, firstErr, federationconfig.ErrHubUnavailable)
	require.Len(t, hub.rotationCalls, 1)
	first := hub.rotationCalls[0]
	pending, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, first.Token, pending.Token)

	hub.rotationErr = nil
	secondErr := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.NoError(t, secondErr)
	require.Len(t, hub.rotationCalls, 2)
	assert.Equal(t, first, hub.rotationCalls[1])
	final, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, "identity-user", final.Actor)
	assert.True(t, final.ManagedByConfig)
}

func TestReconcileMappingCredentialFailureInsideReplicaServiceStaysClassified(t *testing.T) {
	store := openReconcileStore(t)
	credentials := newFakeCredentialStore()
	credentials.storeErr = errors.New("credential backend unavailable")
	credentials.failStoreAt = 1
	hub := newFakeHub()

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrCredentialIO)
	assert.NotContains(t, err.Error(), testCatalogBearer)
	require.Len(t, hub.enrollmentCalls, 1)
	require.Len(t, credentials.managedReserveCalls, 1)
	require.Len(t, credentials.storeCalls, 1)
}

func TestReconcileMappingMatchingBindingAndCredentialIsImmediatelyConverged(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	credentials.credentials[project.UID] = managedCredential()
	hub := newFakeHub()
	wakes := 0

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Empty(t, credentials.storeCalls)
	assert.Zero(t, wakes)
}

func TestReconcileMappingPushRepairDoesNotRotateEnrollment(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	binding, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	binding.PushEnabled = false
	_, err = store.UpsertFederationBinding(context.Background(), binding)
	require.NoError(t, err)

	credentials := newFakeCredentialStore()
	original := managedCredential()
	credentials.credentials[project.UID] = original
	hub := newFakeHub()
	wakes := 0

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)

	require.NoError(t, err)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	repaired, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.True(t, repaired.PushEnabled)
	assert.Equal(t, original.Actor, repaired.Actor)
	final, found := credentials.get(project.UID)
	require.True(t, found)
	assert.Equal(t, original.Token, final.Token)
	assert.Equal(t, original.Actor, final.Actor)
	assert.True(t, final.ManagedByConfig)
	assert.Equal(t, 1, wakes)
}

func TestReconcileMappingIncompleteBindingWithValidCredentialRepairsLocally(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		pushEnabled bool
	}{
		{name: "binding disabled", enabled: false, pushEnabled: true},
		{name: "push disabled", enabled: true, pushEnabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project := createMatchingBoundProject(t, store)
			binding, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			binding.Enabled = tt.enabled
			binding.PushEnabled = tt.pushEnabled
			_, err = store.UpsertFederationBinding(context.Background(), binding)
			require.NoError(t, err)

			credentials := newFakeCredentialStore()
			original := managedCredential()
			credentials.credentials[project.UID] = original
			hub := newFakeHub()
			wakes := 0

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), func() { wakes++ },
			)
			require.NoError(t, err)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)

			repaired, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			assert.True(t, repaired.Enabled)
			assert.True(t, repaired.PushEnabled)
			assert.Equal(t, "identity-user", repaired.Actor)

			final, ok := credentials.get(project.UID)
			require.True(t, ok)
			assert.Equal(t, original.Token, final.Token)
			assert.Equal(t, original.Actor, final.Actor)
			require.Len(t, credentials.storeCalls, 1)
			assert.Equal(t, 1, wakes)
		})
	}
}

func TestReconcileMappingMatchingManualCredentialBecomesManaged(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubCatalog = ""
	manual.HubProjectName = ""
	manual.RequestedActor = ""
	credentials.credentials[project.UID] = manual
	hub := newFakeHub()
	wakes := 0

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	require.Len(t, credentials.storeCalls, 1)
	adopted, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, manual.Token, adopted.Token)
	assert.True(t, adopted.ManagedByConfig)
	assert.Equal(t, "primary", adopted.HubCatalog)
	assert.Equal(t, "hub-project", adopted.HubProjectName)
	assert.Equal(t, "user-a", adopted.RequestedActor)
	assert.Zero(t, wakes)
}

func TestReconcileMappingIncompleteBindingWithManualCredentialAdoptsAndRepairs(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		pushEnabled bool
	}{
		{name: "binding disabled", enabled: false, pushEnabled: true},
		{name: "push disabled", enabled: true, pushEnabled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project := createMatchingBoundProject(t, store)
			binding, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			binding.Enabled = tt.enabled
			binding.PushEnabled = tt.pushEnabled
			_, err = store.UpsertFederationBinding(context.Background(), binding)
			require.NoError(t, err)

			credentials := newFakeCredentialStore()
			manual := managedCredential()
			manual.ManagedByConfig = false
			manual.HubCatalog = ""
			manual.HubProjectName = ""
			manual.RequestedActor = ""
			credentials.credentials[project.UID] = manual
			hub := newFakeHub()
			hub.enrollmentErr = errors.New("manual credential must not be enrolled")
			hub.rotationErr = errors.New("manual credential must not be rotated")
			wakes := 0

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), func() { wakes++ },
			)
			require.NoError(t, err)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)

			repaired, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			assert.True(t, repaired.Enabled)
			assert.True(t, repaired.PushEnabled)
			assert.Equal(t, "identity-user", repaired.Actor)

			final, ok := credentials.get(project.UID)
			require.True(t, ok)
			assert.Equal(t, manual.Token, final.Token)
			assert.Equal(t, "identity-user", final.Actor)
			assert.True(t, final.ManagedByConfig)
			assert.Equal(t, "primary", final.HubCatalog)
			assert.Equal(t, "hub-project", final.HubProjectName)
			assert.Equal(t, "user-a", final.RequestedActor)
			require.Len(t, credentials.storeCalls, 1)
			assert.Equal(t, 1, wakes)
		})
	}
}

func TestReconcileMappingManagedCredentialEmptyBindingActorRepairs(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	_, err := store.ExecContext(
		context.Background(),
		`UPDATE federation_bindings SET bound_actor = '' WHERE project_id = ?`,
		project.ID,
	)
	require.NoError(t, err)

	credentials := newFakeCredentialStore()
	original := managedCredential()
	credentials.credentials[project.UID] = original
	hub := newFakeHub()
	wakes := 0

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), func() { wakes++ },
	)
	require.NoError(t, err)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)

	repaired, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.True(t, repaired.Enabled)
	assert.True(t, repaired.PushEnabled)
	assert.Equal(t, original.Actor, repaired.Actor)
	final, ok := credentials.get(project.UID)
	require.True(t, ok)
	assert.Equal(t, original.Token, final.Token)
	assert.Equal(t, original.Actor, final.Actor)
	assert.True(t, final.ManagedByConfig)
	assert.Equal(t, original.HubCatalog, final.HubCatalog)
	assert.Equal(t, original.HubProjectName, final.HubProjectName)
	assert.Equal(t, original.RequestedActor, final.RequestedActor)
	require.Len(t, credentials.storeCalls, 1)
	assert.Equal(t, 1, wakes)
}

func TestReconcileMappingRejectsManagedMetadataMismatchRegardlessOfBindingCompleteness(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, store *sqlitestore.Store, binding db.FederationBinding)
	}{
		{
			name: "binding disabled",
			mutate: func(t *testing.T, store *sqlitestore.Store, binding db.FederationBinding) {
				binding.Enabled = false
				_, err := store.UpsertFederationBinding(context.Background(), binding)
				require.NoError(t, err)
			},
		},
		{
			name: "push disabled",
			mutate: func(t *testing.T, store *sqlitestore.Store, binding db.FederationBinding) {
				binding.PushEnabled = false
				_, err := store.UpsertFederationBinding(context.Background(), binding)
				require.NoError(t, err)
			},
		},
		{
			name: "actor missing",
			mutate: func(t *testing.T, store *sqlitestore.Store, binding db.FederationBinding) {
				_, err := store.ExecContext(
					context.Background(),
					`UPDATE federation_bindings SET bound_actor = '' WHERE project_id = ?`,
					binding.ProjectID,
				)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project := createMatchingBoundProject(t, store)
			binding, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			tt.mutate(t, store, binding)
			before, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)

			credentials := newFakeCredentialStore()
			mismatched := managedCredential()
			mismatched.HubCatalog = "secondary"
			credentials.credentials[project.UID] = mismatched
			hub := newFakeHub()
			wakes := 0

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), func() { wakes++ },
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
			assert.Empty(t, credentials.storeCalls)
			assert.Empty(t, credentials.deleteCalls)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)
			assert.Zero(t, wakes)

			after, readErr := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, readErr)
			assert.Equal(t, before, after)
			final, ok := credentials.get(project.UID)
			require.True(t, ok)
			assert.Equal(t, mismatched, final)
		})
	}
}

func TestReconcileMappingRejectsNonmatchingManualCredentialWithoutOverwrite(t *testing.T) {
	store := openReconcileStore(t)
	project, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	manual := managedCredential()
	manual.ManagedByConfig = false
	manual.HubURL = "https://other.example"
	manual.Token = "manual-credential-secret"
	credentials.credentials[project.UID] = manual
	hub := newFakeHub()

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	assert.NotContains(t, err.Error(), manual.Token)
	assert.NotContains(t, err.Error(), testCatalogBearer)
	assert.Equal(t, manual, credentials.credentials[project.UID])
	assert.Empty(t, credentials.storeCalls)
	assert.Empty(t, credentials.deleteCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Equal(t, 1, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	_, bindingErr := store.FederationBindingByProject(context.Background(), project.ID)
	assert.ErrorIs(t, bindingErr, db.ErrNotFound)
}

func TestReconcileMappingRejectsHubKeyCredentialMismatchBeforeEnable(t *testing.T) {
	store := openReconcileStore(t)
	_, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	mismatched := managedCredential()
	mismatched.HubProjectID = 99
	credentials.credentials[hubProjectUID] = mismatched
	hub := newFakeHub()

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
	assert.Equal(t, 1, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Empty(t, credentials.storeCalls)
}

func TestReconcileMappingRejectsBindingConflictsWithoutMutation(t *testing.T) {
	tests := []struct {
		name             string
		mutate           func(t *testing.T, store *sqlitestore.Store, project db.Project)
		wantResolveCalls int
	}{
		{
			name: "role",
			mutate: func(t *testing.T, store *sqlitestore.Store, project db.Project) {
				_, err := store.EnableProjectFederation(context.Background(), project.ID, "identity-user")
				require.NoError(t, err)
			},
		},
		{
			name: "origin",
			mutate: func(t *testing.T, store *sqlitestore.Store, project db.Project) {
				upsertSpokeBinding(t, store, project, "https://other.example", hubProjectUID)
			},
		},
		{
			name:             "hub project uid",
			wantResolveCalls: 1,
			mutate: func(t *testing.T, store *sqlitestore.Store, project db.Project) {
				upsertSpokeBinding(t, store, project, "https://hub.example", recreatedProjectUID)
			},
		},
		{
			name:             "hub project id",
			wantResolveCalls: 1,
			mutate: func(t *testing.T, store *sqlitestore.Store, project db.Project) {
				upsertSpokeBinding(t, store, project, "https://hub.example", hubProjectUID)
				binding, err := store.FederationBindingByProject(context.Background(), project.ID)
				require.NoError(t, err)
				binding.HubProjectID = 99
				_, err = store.UpsertFederationBinding(context.Background(), binding)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project, err := store.CreateProject(context.Background(), "spoke-project")
			require.NoError(t, err)
			tt.mutate(t, store, project)
			before, err := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, err)
			credentials := newFakeCredentialStore()
			hub := newFakeHub()
			wakes := 0

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), func() { wakes++ },
			)
			require.Error(t, err)
			assert.ErrorIs(t, err, federationconfig.ErrBindingConflict)
			after, readErr := store.FederationBindingByProject(context.Background(), project.ID)
			require.NoError(t, readErr)
			assert.Equal(t, before, after)
			assert.Empty(t, credentials.storeCalls)
			assert.Empty(t, credentials.deleteCalls)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)
			assert.Equal(t, tt.wantResolveCalls, hub.resolveCalls)
			assert.Zero(t, hub.ensureCalls)
			assert.Zero(t, wakes)
		})
	}
}

func TestReconcileMappingRecreatedSameNameHubProjectUIDConflicts(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	before, err := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	credentials.credentials[project.UID] = managedCredential()
	hub := newFakeHub()
	hub.project.UID = recreatedProjectUID

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrBindingConflict)
	after, readErr := store.FederationBindingByProject(context.Background(), project.ID)
	require.NoError(t, readErr)
	assert.Equal(t, before, after)
	assert.Empty(t, credentials.storeCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Equal(t, 1, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
}

func TestReconcileMappingBoundProjectMissingOnHubDoesNotCreate(t *testing.T) {
	store := openReconcileStore(t)
	project := createMatchingBoundProject(t, store)
	credentials := newFakeCredentialStore()
	credentials.credentials[project.UID] = managedCredential()
	hub := newFakeHub()
	hub.resolveProjectErr = &federationconfig.HubError{
		Kind:       federationconfig.ErrHubValidation,
		Operation:  "resolve project",
		StatusCode: http.StatusNotFound,
	}

	err := federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrBindingConflict)
	assert.Equal(t, 1, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
}

func TestReconcileMappingStaleHubUIDReservationConflicts(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeHub)
	}{
		{
			name: "same name recreated",
			configure: func(hub *fakeHub) {
				hub.project.ID = 84
				hub.project.UID = recreatedProjectUID
			},
		},
		{
			name: "old project missing",
			configure: func(hub *fakeHub) {
				hub.resolveProjectErr = &federationconfig.HubError{
					Kind:       federationconfig.ErrHubValidation,
					Operation:  "resolve project",
					StatusCode: http.StatusNotFound,
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openReconcileStore(t)
			project, err := store.CreateProject(context.Background(), "spoke-project")
			require.NoError(t, err)
			credentials := newFakeCredentialStore()
			pending := managedCredential()
			pending.Actor = ""
			pending.Token = "pending-token-a"
			credentials.credentials[hubProjectUID] = pending
			hub := newFakeHub()
			tt.configure(hub)

			err = federationconfig.ReconcileMapping(
				context.Background(), store, credentials, hub,
				testCatalog(), testMapping(), nil,
			)

			require.ErrorIs(t, err, federationconfig.ErrConfigurationConflict)
			assert.Equal(t, 1, hub.resolveCalls)
			assert.Zero(t, hub.ensureCalls)
			assert.Empty(t, hub.enrollmentCalls)
			assert.Empty(t, hub.rotationCalls)
			assert.Empty(t, credentials.managedReserveCalls)
			unchanged, readErr := store.ProjectByName(context.Background(), project.Name)
			require.NoError(t, readErr)
			assert.Equal(t, project.UID, unchanged.UID)
			_, bindingErr := store.FederationBindingByProject(
				context.Background(), project.ID,
			)
			assert.ErrorIs(t, bindingErr, db.ErrNotFound)
			final, found := credentials.get(hubProjectUID)
			require.True(t, found)
			assert.Equal(t, pending, final)
			_, newFound, readErr := credentials.FederationCredential(
				context.Background(), recreatedProjectUID,
			)
			require.NoError(t, readErr)
			assert.False(t, newFound)
		})
	}
}

func TestReconcileMappingHubUIDOwnedByAnotherLocalProjectConflictsBeforeEnable(t *testing.T) {
	store := openReconcileStore(t)
	_, err := store.CreateProjectWithUID(context.Background(), "other-project", hubProjectUID)
	require.NoError(t, err)
	local, err := store.CreateProject(context.Background(), "spoke-project")
	require.NoError(t, err)
	credentials := newFakeCredentialStore()
	hub := newFakeHub()

	err = federationconfig.ReconcileMapping(
		context.Background(), store, credentials, hub,
		testCatalog(), testMapping(), nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, federationconfig.ErrBindingConflict)
	assert.Equal(t, 1, hub.resolveCalls)
	assert.Zero(t, hub.ensureCalls)
	assert.Empty(t, hub.enrollmentCalls)
	assert.Empty(t, hub.rotationCalls)
	assert.Empty(t, credentials.managedReserveCalls)
	unchanged, readErr := store.ProjectByName(context.Background(), local.Name)
	require.NoError(t, readErr)
	assert.Equal(t, local.UID, unchanged.UID)
}

func TestFederationConfigReconcilerProcessesDueMappingsInConfigOrder(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC))
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": {federationconfig.ErrHubUnavailable},
		"hub-b": {federationconfig.ErrConfigurationConflict},
		"hub-c": {&federationconfig.HubError{
			Kind:       federationconfig.ErrHubAuthentication,
			Operation:  "authenticate",
			StatusCode: http.StatusUnauthorized,
		}},
	})
	reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(),
		schedulerTarget("hub-a"), schedulerTarget("hub-b"), schedulerTarget("hub-c"))
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	require.Eventually(t, func() bool {
		return len(factory.snapshotCalls()) == 3
	}, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		return reconciler.Health().LastErrorCategory == "hub_authentication"
	}, time.Second, time.Millisecond)
	calls := factory.snapshotCalls()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"hub-a", "hub-b", "hub-c"},
		[]string{calls[0].name, calls[1].name, calls[2].name})
	assert.True(t, calls[0].at.Equal(clock.start))
	assert.True(t, calls[1].at.Equal(clock.start))
	assert.True(t, calls[2].at.Equal(clock.start))

	health := reconciler.Health()
	assert.Equal(t, 3, health.Configured)
	assert.Zero(t, health.Reconciled)
	assert.Equal(t, 2, health.Pending)
	assert.Equal(t, 1, health.Conflicted)
	require.NotNil(t, health.LastAttemptAt)
	assert.True(t, health.LastAttemptAt.Equal(clock.start))
	assert.Equal(t, "hub_authentication", health.LastErrorCategory)
	assert.Equal(t, http.StatusUnauthorized, health.LastErrorStatus)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFederationConfigReconcilerMaintainsIndependentBackoffAndQuietsSuccess(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 1, 30, 0, 0, time.UTC))
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": {
			federationconfig.ErrHubUnavailable,
			federationconfig.ErrHubUnavailable,
			nil,
		},
		"hub-b": {
			federationconfig.ErrHubUnavailable,
			nil,
		},
	})
	reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(),
		schedulerTarget("hub-a"), schedulerTarget("hub-b"))
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForFactoryCalls(t, factory, 2)
	waitForTimerCount(t, clock, 1)
	clock.Advance(time.Second)
	waitForFactoryCalls(t, factory, 4)
	waitForTimerCount(t, clock, 2)
	clock.Advance(2 * time.Second)
	waitForFactoryCalls(t, factory, 5)
	waitForReconciled(t, reconciler, 2)

	calls := factory.snapshotCalls()
	require.Len(t, calls, 5)
	assert.Equal(t,
		[]string{"hub-a", "hub-b", "hub-a", "hub-b", "hub-a"},
		[]string{calls[0].name, calls[1].name, calls[2].name, calls[3].name, calls[4].name})
	assert.True(t, calls[0].at.Equal(clock.start))
	assert.True(t, calls[1].at.Equal(clock.start))
	assert.True(t, calls[2].at.Equal(clock.start.Add(time.Second)))
	assert.True(t, calls[3].at.Equal(clock.start.Add(time.Second)))
	assert.True(t, calls[4].at.Equal(clock.start.Add(3*time.Second)))

	health := reconciler.Health()
	assert.Equal(t, federationconfig.Health{
		Configured:    2,
		Reconciled:    2,
		LastAttemptAt: timePointer(clock.start.Add(3 * time.Second)),
		LastSuccessAt: timePointer(clock.start.Add(3 * time.Second)),
	}, health)

	clock.Advance(24 * time.Hour)
	assert.Never(t, func() bool {
		return len(factory.snapshotCalls()) != 5
	}, 20*time.Millisecond, time.Millisecond)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFederationConfigReconcilerRecordsSuccessAtCompletion(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 1, 45, 0, 0, time.UTC))
	store := openReconcileStore(t)
	reconciler := federationconfig.NewReconciler(federationconfig.ReconcilerConfig{
		Store:       store,
		Credentials: newFakeCredentialStore(),
		Targets:     []federationconfig.Target{schedulerTarget("hub-a")},
		HubFactory: func(
			_ context.Context, _ config.CatalogDaemonConfig,
		) (federationconfig.Hub, error) {
			clock.Advance(2 * time.Second)
			hub := newFakeHub()
			hub.project.UID = "01HZNQ7VFPK1XGD8R5MABCD4E1"
			return hub, nil
		},
		Clock:  clock,
		Logger: ioDiscardLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForReconciled(t, reconciler, 1)
	health := reconciler.Health()
	require.NotNil(t, health.LastAttemptAt)
	assert.True(t, health.LastAttemptAt.Equal(clock.start))
	require.NotNil(t, health.LastSuccessAt)
	assert.True(t, health.LastSuccessAt.Equal(clock.start.Add(2*time.Second)))

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFederationConfigReconcilerCapsBackoffAtFiveMinutes(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 2, 0, 0, 0, time.UTC))
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": repeatError(federationconfig.ErrHubUnavailable, 20),
	})
	reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(), schedulerTarget("hub-a"))
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	wantDelays := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	waitForFactoryCalls(t, factory, 1)
	for i, delay := range wantDelays {
		waitForTimerCount(t, clock, i+1)
		clock.Advance(delay)
		waitForFactoryCalls(t, factory, i+2)
	}
	assert.Equal(t, wantDelays, clock.snapshotDurations()[:len(wantDelays)])

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFederationConfigReconcilerRetriesEveryErrorCategory(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantCategory string
		wantStatus   int
		wantConflict int
	}{
		{name: "configuration conflict", err: federationconfig.ErrConfigurationConflict, wantCategory: "configuration_conflict", wantConflict: 1},
		{name: "binding conflict", err: federationconfig.ErrBindingConflict, wantCategory: "binding_conflict", wantConflict: 1},
		{name: "credential io", err: federationconfig.ErrCredentialIO, wantCategory: "credential_io"},
		{name: "hub unavailable", err: federationconfig.ErrHubUnavailable, wantCategory: "hub_unavailable"},
		{
			name: "hub authentication",
			err: &federationconfig.HubError{
				Kind:       federationconfig.ErrHubAuthentication,
				Operation:  "authenticate",
				StatusCode: http.StatusForbidden,
			},
			wantCategory: "hub_authentication",
			wantStatus:   http.StatusForbidden,
		},
		{name: "hub validation", err: federationconfig.ErrHubValidation, wantCategory: "hub_validation"},
		{name: "local storage", err: federationconfig.ErrLocalStorage, wantCategory: "local_storage"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newManualClock(time.Date(2026, 7, 23, 2, 30, 0, 0, time.UTC))
			factory := newScriptedHubFactory(clock, map[string][]error{
				"hub-a": {tt.err, nil},
			})
			reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(), schedulerTarget("hub-a"))
			ctx, cancel := context.WithCancel(context.Background())
			done := runReconciler(ctx, t, reconciler)

			waitForFactoryCalls(t, factory, 1)
			require.Eventually(t, func() bool {
				return reconciler.Health().LastErrorCategory == tt.wantCategory
			}, time.Second, time.Millisecond)
			health := reconciler.Health()
			assert.Equal(t, tt.wantCategory, health.LastErrorCategory)
			assert.Equal(t, tt.wantStatus, health.LastErrorStatus)
			assert.Equal(t, tt.wantConflict, health.Conflicted)
			assert.Equal(t, 1-tt.wantConflict, health.Pending)

			waitForTimerCount(t, clock, 1)
			clock.Advance(time.Second)
			waitForFactoryCalls(t, factory, 2)
			waitForReconciled(t, reconciler, 1)
			health = reconciler.Health()
			assert.Equal(t, 1, health.Reconciled)
			assert.Zero(t, health.Pending)
			assert.Zero(t, health.Conflicted)
			assert.Empty(t, health.LastErrorCategory)
			assert.Zero(t, health.LastErrorStatus)

			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
		})
	}
}

func TestClassifyReconciliationErrorUsesInternalForUnknown(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 3, 15, 0, 0, time.UTC))
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": {errors.New("planted internal failure")},
	})
	reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(), schedulerTarget("hub-a"))
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForFactoryCalls(t, factory, 1)
	require.Eventually(t, func() bool {
		return reconciler.Health().LastErrorCategory != ""
	}, time.Second, time.Millisecond)
	health := reconciler.Health()
	assert.Equal(t, "internal", health.LastErrorCategory)
	assert.Zero(t, health.LastErrorStatus)

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestFederationConfigReconcilerCancellationStopsTimerAndRun(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 3, 0, 0, 0, time.UTC))
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": {federationconfig.ErrHubUnavailable},
	})
	reconciler := newTestReconciler(t, clock, factory, ioDiscardLogger(), schedulerTarget("hub-a"))
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForFactoryCalls(t, factory, 1)
	waitForTimerCount(t, clock, 1)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	assert.Equal(t, 1, clock.stoppedTimerCount())
}

func TestFederationConfigReconcilerLogsOnlySanitizedTransitions(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 3, 30, 0, 0, time.UTC))
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	factory := newScriptedHubFactory(clock, map[string][]error{
		"hub-a": {
			&federationconfig.HubError{
				Kind:       federationconfig.ErrHubUnavailable,
				Operation:  "response reflected secret-body",
				StatusCode: http.StatusServiceUnavailable,
			},
			&federationconfig.HubError{
				Kind:       federationconfig.ErrHubUnavailable,
				Operation:  "response reflected secret-body",
				StatusCode: http.StatusServiceUnavailable,
			},
			&federationconfig.HubError{
				Kind:       federationconfig.ErrHubAuthentication,
				Operation:  "response reflected secret-body",
				StatusCode: http.StatusUnauthorized,
			},
			nil,
		},
	})
	target := schedulerTarget("hub-a")
	target.Catalog.URL = "https://sensitive-hub.example/private"
	target.Catalog.Token = "catalog-secret"
	target.Mapping.Actor = "sensitive-actor"
	reconciler := newTestReconciler(t, clock, factory, logger, target)
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForFactoryCalls(t, factory, 1)
	waitForTimerCount(t, clock, 1)
	clock.Advance(time.Second)
	waitForFactoryCalls(t, factory, 2)
	waitForTimerCount(t, clock, 2)
	clock.Advance(2 * time.Second)
	waitForFactoryCalls(t, factory, 3)
	waitForTimerCount(t, clock, 3)
	clock.Advance(4 * time.Second)
	waitForFactoryCalls(t, factory, 4)
	waitForReconciled(t, reconciler, 1)

	got := logs.String()
	assert.Equal(t, 3, bytes.Count([]byte(got), []byte("\n")))
	assert.Contains(t, got, "state=pending category=hub_unavailable status=503")
	assert.Contains(t, got, "state=pending category=hub_authentication status=401")
	assert.Contains(t, got, "state=reconciled category= status=0")
	for _, secret := range []string{
		"secret-body", "sensitive-hub.example", "catalog-secret",
		"sensitive-actor",
	} {
		assert.NotContains(t, got, secret)
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestReconcilerTransitionLogsIncludeMappingCoordinatesWithoutSecrets(t *testing.T) {
	clock := newManualClock(time.Date(2026, 7, 23, 3, 45, 0, 0, time.UTC))
	var logs bytes.Buffer
	logger := log.New(&logs, "", 0)
	factory := newScriptedHubFactory(clock, map[string][]error{
		"primary": {
			&federationconfig.HubError{
				Kind:      federationconfig.ErrConfigurationConflict,
				Operation: "raw-body-marker header-marker token-marker url-marker actor-marker",
			},
		},
		"secondary": {federationconfig.ErrConfigurationConflict},
	})
	primary := federationconfig.Target{
		Catalog: config.CatalogDaemonConfig{
			Name: "primary", URL: "https://url-marker.example/private", Token: "token-marker",
		},
		Mapping: config.FederationProjectConfig{
			Hub: "primary", SpokeProject: "spoke-project", HubProject: "hub-project", Actor: "actor-marker",
		},
	}
	secondary := schedulerTarget("secondary")
	reconciler := newTestReconciler(t, clock, factory, logger, primary, secondary)
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, t, reconciler)

	waitForFactoryCalls(t, factory, 2)
	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "hub=primary")
	}, time.Second, time.Millisecond)

	got := logs.String()
	assert.Contains(t, got,
		"hub=primary spoke_project=spoke-project hub_project=hub-project state=conflict category=configuration_conflict status=0")
	for _, secret := range []string{
		"url-marker", "token-marker", "actor-marker", "header-marker", "raw-body-marker",
	} {
		assert.NotContains(t, got, secret)
	}

	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

type manualClock struct {
	mu        sync.Mutex
	start     time.Time
	now       time.Time
	timers    []*manualTimer
	durations []time.Duration
}

type manualTimer struct {
	clock   *manualClock
	at      time.Time
	ch      chan time.Time
	fired   bool
	stopped bool
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{start: now, now: now}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(delay time.Duration) federationconfig.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &manualTimer{
		clock: c,
		at:    c.now.Add(delay),
		ch:    make(chan time.Time, 1),
	}
	c.timers = append(c.timers, timer)
	c.durations = append(c.durations, delay)
	return timer
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now
	var due []*manualTimer
	for _, timer := range c.timers {
		if !timer.fired && !timer.stopped && !timer.at.After(now) {
			timer.fired = true
			due = append(due, timer)
		}
	}
	c.mu.Unlock()
	for _, timer := range due {
		timer.ch <- now
	}
}

func (c *manualClock) snapshotDurations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.durations...)
}

func (c *manualClock) stoppedTimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var stopped int
	for _, timer := range c.timers {
		if timer.stopped {
			stopped++
		}
	}
	return stopped
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }

func (t *manualTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.fired || t.stopped {
		return false
	}
	t.stopped = true
	return true
}

type factoryCall struct {
	name string
	at   time.Time
}

type scriptedHubFactory struct {
	mu      sync.Mutex
	clock   *manualClock
	scripts map[string][]error
	calls   []factoryCall
}

func newScriptedHubFactory(
	clock *manualClock, scripts map[string][]error,
) *scriptedHubFactory {
	return &scriptedHubFactory{clock: clock, scripts: scripts}
}

func (f *scriptedHubFactory) NewHub(
	_ context.Context, catalog config.CatalogDaemonConfig,
) (federationconfig.Hub, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := 0
	for _, call := range f.calls {
		if call.name == catalog.Name {
			index++
		}
	}
	f.calls = append(f.calls, factoryCall{name: catalog.Name, at: f.clock.Now()})
	if script := f.scripts[catalog.Name]; index < len(script) && script[index] != nil {
		return nil, script[index]
	}
	hub := newFakeHub()
	switch catalog.Name {
	case "hub-a":
		hub.project.UID = "01HZNQ7VFPK1XGD8R5MABCD4E1"
	case "hub-b":
		hub.project.UID = "01HZNQ7VFPK1XGD8R5MABCD4E2"
	case "hub-c":
		hub.project.UID = "01HZNQ7VFPK1XGD8R5MABCD4E3"
	}
	return hub, nil
}

func (f *scriptedHubFactory) snapshotCalls() []factoryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factoryCall(nil), f.calls...)
}

func schedulerTarget(name string) federationconfig.Target {
	return federationconfig.Target{
		Catalog: config.CatalogDaemonConfig{
			Name: name,
			URL:  "https://" + name + ".example",
		},
		Mapping: config.FederationProjectConfig{
			Hub:          name,
			SpokeProject: "spoke-" + name,
			HubProject:   "project-" + name,
			Actor:        "user-" + name,
		},
	}
}

func newTestReconciler(
	t *testing.T,
	clock *manualClock,
	factory *scriptedHubFactory,
	logger *log.Logger,
	targets ...federationconfig.Target,
) *federationconfig.Reconciler {
	t.Helper()
	store := openReconcileStore(t)
	return federationconfig.NewReconciler(federationconfig.ReconcilerConfig{
		Store:       store,
		Credentials: newFakeCredentialStore(),
		Targets:     targets,
		HubFactory:  factory.NewHub,
		Clock:       clock,
		Logger:      logger,
	})
}

func runReconciler(
	ctx context.Context, t *testing.T, reconciler *federationconfig.Reconciler,
) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- reconciler.Run(ctx)
	}()
	return done
}

func waitForFactoryCalls(t *testing.T, factory *scriptedHubFactory, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(factory.snapshotCalls()) == count
	}, time.Second, time.Millisecond)
}

func waitForTimerCount(t *testing.T, clock *manualClock, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return len(clock.snapshotDurations()) == count
	}, time.Second, time.Millisecond)
}

func waitForReconciled(
	t *testing.T, reconciler *federationconfig.Reconciler, count int,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		return reconciler.Health().Reconciled == count
	}, time.Second, time.Millisecond)
}

func repeatError(err error, count int) []error {
	result := make([]error, count)
	for i := range result {
		result[i] = err
	}
	return result
}

func ioDiscardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func newFakeHub() *fakeHub {
	return &fakeHub{
		project: federationconfig.HubProject{
			ID: 42, UID: hubProjectUID, Name: "hub-project",
			ReplayHorizonEventID: 9, BaselineThroughEventID: 6,
		},
		enrollment: federationconfig.Enrollment{ID: 71, Actor: "identity-user"},
	}
}

func testCatalog() config.CatalogDaemonConfig {
	return config.CatalogDaemonConfig{
		Name: "primary", URL: "https://hub.example", Token: testCatalogBearer,
	}
}

func testMapping() config.FederationProjectConfig {
	return config.FederationProjectConfig{
		Hub: "primary", SpokeProject: "spoke-project",
		HubProject: "hub-project", Actor: "user-a",
	}
}

func managedCredential() config.FederationCredential {
	return config.FederationCredential{
		HubURL: "https://HUB.EXAMPLE:443/path", HubProjectID: 42,
		Token: "managed-enrollment-secret", Capabilities: "claim,pull,push",
		Actor: "identity-user", ManagedByConfig: true,
		HubCatalog: "primary", HubProjectName: "hub-project", RequestedActor: "user-a",
		SpokeProjectName: "spoke-project",
	}
}

func createMatchingBoundProject(t *testing.T, store *sqlitestore.Store) db.Project {
	t.Helper()
	project, err := store.CreateProjectWithUID(context.Background(), "spoke-project", hubProjectUID)
	require.NoError(t, err)
	upsertSpokeBinding(t, store, project, "https://HUB.EXAMPLE:443/path", hubProjectUID)
	return project
}

func upsertSpokeBinding(
	t *testing.T, store *sqlitestore.Store, project db.Project, hubURL, projectUID string,
) {
	t.Helper()
	_, err := store.UpsertFederationBinding(context.Background(), db.FederationBinding{
		ProjectID: project.ID, Role: db.FederationRoleSpoke,
		HubURL: hubURL, HubProjectID: 42, HubProjectUID: projectUID,
		ReplayHorizonEventID: 9, PullCursorEventID: 8,
		PushEnabled: true, Actor: "identity-user", Enabled: true,
	})
	require.NoError(t, err)
}

func openReconcileStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	unsetReconcileAuthToken(t)
	t.Setenv("KATA_HOME", t.TempDir())
	store, err := sqlitestore.Open(context.Background(), filepath.Join(t.TempDir(), "kata.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func unsetReconcileAuthToken(t *testing.T) {
	t.Helper()
	value, existed := os.LookupEnv("KATA_AUTH_TOKEN")
	require.NoError(t, os.Unsetenv("KATA_AUTH_TOKEN"))
	t.Cleanup(func() {
		if existed {
			require.NoError(t, os.Setenv("KATA_AUTH_TOKEN", value))
		} else {
			require.NoError(t, os.Unsetenv("KATA_AUTH_TOKEN"))
		}
	})
}

var (
	_ federationconfig.Hub             = (*fakeHub)(nil)
	_ config.FederationCredentialStore = (*fakeCredentialStore)(nil)
)
