package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

var (
	// ErrFederationReplicaInvalidInput classifies invalid service parameters.
	ErrFederationReplicaInvalidInput = errors.New("invalid federation replica input")
	// ErrFederationReplicaBindingConflict classifies an incompatible existing binding.
	ErrFederationReplicaBindingConflict = errors.New("federation replica binding conflict")
	// ErrFederationReplicaCredentialConflict classifies a credential targeting another hub.
	ErrFederationReplicaCredentialConflict = errors.New("federation replica credential conflict")
	// ErrFederationReplicaReservationChanged identifies an exact managed
	// reservation removed or replaced while its hub request was in flight.
	ErrFederationReplicaReservationChanged = fmt.Errorf(
		"%w: managed reservation changed",
		ErrFederationReplicaCredentialConflict,
	)
	// ErrFederationReplicaLeavePending identifies a project whose explicit
	// leave intent blocks new config-driven hub operations.
	ErrFederationReplicaLeavePending = fmt.Errorf(
		"%w: explicit leave pending",
		ErrFederationReplicaCredentialConflict,
	)
	// ErrFederationReplicaCredentialIO classifies bounded credential-store failures.
	ErrFederationReplicaCredentialIO = errors.New("federation replica credential I/O")

	errFederationReplicaCapabilityMismatch = fmt.Errorf(
		"%w: capability mismatch", ErrFederationReplicaInvalidInput,
	)
	errFederationReplicaProjectCollision   = errors.New("federation replica project collision")
	errFederationReplicaProjectNotFound    = errors.New("federation replica project not found")
	errFederationReplicaRejoinNameMismatch = errors.New("federation replica rejoin name mismatch")
	errFederationReplicaIssueSyncConflict  = errors.New("federation replica issue-sync conflict")
)

// ensureFederationReplicaMu serializes replica ensure, reservation, and leave
// state transitions. The lock is process-wide because a replica may be
// discovered through either its shared UID or an adoption target name, so
// there is no single stable key before the lookups complete. Credential stores
// do not call this service, so the lock order (service, then credential store)
// has no reverse edge.
var ensureFederationReplicaMu sync.Mutex

type federationReplicaHubOperationState struct {
	count int
	done  chan struct{}
}

var (
	federationReplicaHubOperations = make(map[string]*federationReplicaHubOperationState)
	federationReplicaLeaveIntents  = make(map[string]struct{})
	federationReplicaSuppressionMu sync.RWMutex
	federationReplicaSuppressed    = make(map[string]struct{})
)

// FederationReplicaError is a classified application error returned by
// EnsureFederationReplica. HTTP handlers translate it into the public wire
// status and error code at the transport boundary.
type FederationReplicaError struct {
	kind    error
	message string
	hint    string
}

// Error implements error.
func (e *FederationReplicaError) Error() string {
	return e.message
}

// Unwrap exposes the stable sentinel category to errors.Is.
func (e *FederationReplicaError) Unwrap() error {
	return e.kind
}

// Hint returns the actionable recovery guidance associated with the error.
func (e *FederationReplicaError) Hint() string {
	return e.hint
}

// FederationReplicaManagedReservation identifies the managed credential that
// must still exist unchanged after the caller finishes hub I/O.
type FederationReplicaManagedReservation struct {
	ProjectUID string
	Expected   config.FederationCredential
}

// EnsureFederationReplicaParams describes a local spoke replica to create,
// rejoin, or adopt without depending on HTTP request types.
type EnsureFederationReplicaParams struct {
	HubURL, HubProjectUID, ProjectName string
	HubProjectID                       int64
	ReplayHorizonEventID               int64
	Credential                         config.FederationCredential
	CredentialRekey                    *FederationReplicaCredentialRekeySource
	ManagedReservation                 *FederationReplicaManagedReservation
	PushEnabled, AdoptExisting         bool
}

// FederationReplicaCredentialRekeySource identifies the standalone credential
// that must be moved to the hub UID before UID-changing adoption.
type FederationReplicaCredentialRekeySource struct {
	ProjectUID string
	Expected   config.FederationCredential
}

// ReserveFederationReplicaCredentialParams describes one config-managed
// credential reservation that must serialize with manual replica joins.
type ReserveFederationReplicaCredentialParams struct {
	HubProjectUID string
	ProjectName   string
	Credential    config.FederationCredential
	ExpectedBound bool
}

// PrepareFederationReplicaLeaveResult reports the durable managed leave marker
// after every earlier hub operation for the project has drained.
type PrepareFederationReplicaLeaveResult struct {
	ManagedReservation      config.FederationManagedCredentialReservation
	ManagedReservationFound bool
}

// FinishFederationReplicaHubOperation records a completed enrollment for
// durable leave cleanup before releasing the operation drain.
type FinishFederationReplicaHubOperation func(context.Context, int64) (bool, error)

// BeginFederationReplicaHubOperation registers one config-driven enrollment
// or rotation before network I/O. The returned function must always be called.
func BeginFederationReplicaHubOperation(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectName string,
	baseline config.FederationManagedCredentialReservation,
) (FinishFederationReplicaHubOperation, error) {
	return beginFederationReplicaHubOperation(
		ctx, store, credentials, projectName, baseline, false,
	)
}

// BeginFederationReplicaLeaveRecovery registers exact-token replay used to
// recover an enrollment ID after a crash interrupted explicit leave.
func BeginFederationReplicaLeaveRecovery(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectName string,
	baseline config.FederationManagedCredentialReservation,
) (FinishFederationReplicaHubOperation, error) {
	return beginFederationReplicaHubOperation(
		ctx, store, credentials, projectName, baseline, true,
	)
}

func beginFederationReplicaHubOperation(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectName string,
	baseline config.FederationManagedCredentialReservation,
	recoverPendingLeave bool,
) (FinishFederationReplicaHubOperation, error) {
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return nil, err
	}
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()

	key := federationReplicaTransitionKey(store, projectName)
	if _, pending := federationReplicaLeaveIntents[key]; pending {
		return nil, federationReplicaError(
			ErrFederationReplicaLeavePending,
			"explicit federation leave is pending",
			"",
		)
	}
	if federationReplicaIsSuppressed(key) {
		return nil, federationReplicaError(
			ErrFederationReplicaLeavePending,
			"federation mapping was explicitly left",
			"",
		)
	}
	current, found, err := credentials.FederationCredential(ctx, baseline.ProjectUID)
	if err != nil {
		return nil, credentialIOError("read federation credential before hub operation")
	}
	validPendingState := recoverPendingLeave &&
		current.LeavePending &&
		current.PendingEnrollmentID == 0
	if !found || current != baseline.Credential ||
		(recoverPendingLeave && !validPendingState) ||
		(!recoverPendingLeave && current.LeavePending) {
		return nil, federationReplicaError(
			ErrFederationReplicaReservationChanged,
			"federation credential changed before contacting the hub",
			"",
		)
	}
	state := federationReplicaHubOperations[key]
	if state == nil {
		state = &federationReplicaHubOperationState{done: make(chan struct{})}
		federationReplicaHubOperations[key] = state
	}
	state.count++
	var once sync.Once
	var (
		leavePending bool
		finishErr    error
	)
	return func(finishCtx context.Context, enrollmentID int64) (bool, error) {
		once.Do(func() {
			ensureFederationReplicaMu.Lock()
			defer ensureFederationReplicaMu.Unlock()
			match, found, err := managed.FindManagedFederationCredential(
				finishCtx, projectName,
			)
			if err != nil {
				finishErr = credentialIOError(
					"read managed reservation after hub operation",
				)
			} else if found && match.Credential.LeavePending {
				leavePending = true
				if enrollmentID > 0 {
					replacement := match
					replacement.Credential.PendingEnrollmentID = enrollmentID
					if err := managed.ReplaceManagedFederationCredential(
						finishCtx, match, replacement,
					); err != nil {
						finishErr = credentialIOError(
							"record pending enrollment after hub operation",
						)
					}
				}
			} else {
				_, leavePending = federationReplicaLeaveIntents[key]
			}
			state := federationReplicaHubOperations[key]
			if state == nil {
				return
			}
			state.count--
			if state.count == 0 {
				delete(federationReplicaHubOperations, key)
				close(state.done)
			}
		})
		return leavePending, finishErr
	}, nil
}

// PrepareFederationReplicaLeave durably marks a managed reservation as leaving
// and waits until every earlier hub operation for the project has completed.
func PrepareFederationReplicaLeave(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectID int64,
) (PrepareFederationReplicaLeaveResult, error) {
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return PrepareFederationReplicaLeaveResult{}, err
	}
	project, err := store.ProjectByID(ctx, projectID)
	if err != nil {
		return PrepareFederationReplicaLeaveResult{}, fmt.Errorf(
			"read federation replica project before leave preparation: %w", err,
		)
	}
	key := federationReplicaTransitionKey(store, project.Name)

	ensureFederationReplicaMu.Lock()
	federationReplicaLeaveIntents[key] = struct{}{}
	setFederationReplicaSuppressed(key, true)
	match, found, err := managed.FindManagedFederationCredential(ctx, project.Name)
	if err != nil {
		delete(federationReplicaLeaveIntents, key)
		setFederationReplicaSuppressed(key, false)
		ensureFederationReplicaMu.Unlock()
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return PrepareFederationReplicaLeaveResult{}, err
		}
		return PrepareFederationReplicaLeaveResult{}, credentialIOError(
			"read managed reservation before leave preparation",
		)
	}
	if found && !match.Credential.LeavePending {
		replacement := match
		replacement.Credential.LeavePending = true
		replacement.Credential.PendingEnrollmentID = 0
		if err := managed.ReplaceManagedFederationCredential(ctx, match, replacement); err != nil {
			delete(federationReplicaLeaveIntents, key)
			setFederationReplicaSuppressed(key, false)
			ensureFederationReplicaMu.Unlock()
			if errors.Is(err, config.ErrFederationCredentialConflict) {
				return PrepareFederationReplicaLeaveResult{}, err
			}
			return PrepareFederationReplicaLeaveResult{}, credentialIOError(
				"mark managed reservation for leave",
			)
		}
		match = replacement
	}
	ensureFederationReplicaMu.Unlock()

	for {
		ensureFederationReplicaMu.Lock()
		state := federationReplicaHubOperations[key]
		if state == nil {
			match, found, err = managed.FindManagedFederationCredential(ctx, project.Name)
			ensureFederationReplicaMu.Unlock()
			if err != nil {
				if errors.Is(err, config.ErrFederationCredentialConflict) {
					return PrepareFederationReplicaLeaveResult{}, err
				}
				return PrepareFederationReplicaLeaveResult{}, credentialIOError(
					"read prepared managed reservation",
				)
			}
			return PrepareFederationReplicaLeaveResult{
				ManagedReservation:      match,
				ManagedReservationFound: found,
			}, nil
		}
		done := state.done
		ensureFederationReplicaMu.Unlock()
		select {
		case <-ctx.Done():
			return PrepareFederationReplicaLeaveResult{}, ctx.Err()
		case <-done:
		}
	}
}

func federationReplicaTransitionKey(store db.Storage, projectName string) string {
	return store.InstanceUID() + "\x00" + strings.TrimSpace(projectName)
}

// FederationReplicaMappingSuppressed reports whether explicit leave completed
// for this mapping in the current daemon process.
func FederationReplicaMappingSuppressed(store db.Storage, projectName string) bool {
	return federationReplicaIsSuppressed(federationReplicaTransitionKey(store, projectName))
}

func federationReplicaIsSuppressed(key string) bool {
	federationReplicaSuppressionMu.RLock()
	defer federationReplicaSuppressionMu.RUnlock()
	_, suppressed := federationReplicaSuppressed[key]
	return suppressed
}

func setFederationReplicaSuppressed(key string, suppressed bool) {
	federationReplicaSuppressionMu.Lock()
	defer federationReplicaSuppressionMu.Unlock()
	if suppressed {
		federationReplicaSuppressed[key] = struct{}{}
		return
	}
	delete(federationReplicaSuppressed, key)
}

// LeaveFederationReplica detaches a spoke replica and removes its local
// credential. A retry after a successful detach still attempts credential
// cleanup, and wake fires only after both steps succeed.
func LeaveFederationReplica(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	wake func(),
	projectID int64,
) (db.LeaveFederationResult, error) {
	result, err := leaveFederationReplicaState(ctx, store, credentials, projectID)
	if err != nil {
		return db.LeaveFederationResult{}, err
	}
	if wake != nil {
		wake()
	}
	return result, nil
}

func leaveFederationReplicaState(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectID int64,
) (db.LeaveFederationResult, error) {
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return db.LeaveFederationResult{}, err
	}

	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()

	project, err := store.ProjectByID(ctx, projectID)
	if err != nil {
		return db.LeaveFederationResult{}, fmt.Errorf(
			"read federation replica project before leave: %w", err,
		)
	}
	match, managedReservationFound, err := managed.FindManagedFederationCredential(
		ctx, project.Name,
	)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return db.LeaveFederationResult{}, err
		}
		return db.LeaveFederationResult{}, credentialIOError(
			"read managed reservation before leave",
		)
	}

	// The handler's early role check protects archive-before-detach. Repeat it
	// in the serialized state transition so an ensure or another leave cannot
	// change the binding between validation, detach, and credential cleanup.
	binding, err := store.FederationBindingByProject(ctx, projectID)
	switch {
	case err == nil && binding.Role != db.FederationRoleSpoke:
		return db.LeaveFederationResult{}, db.ErrFederationNotSpoke
	case err != nil && !errors.Is(err, db.ErrNotFound):
		return db.LeaveFederationResult{}, fmt.Errorf(
			"read federation replica binding before leave: %w", err,
		)
	}

	result, err := store.LeaveFederationReplica(ctx, projectID)
	if err != nil {
		return db.LeaveFederationResult{}, err
	}
	if managedReservationFound {
		if err := managed.DeleteManagedFederationCredential(ctx, match); err != nil {
			if errors.Is(err, config.ErrFederationCredentialConflict) {
				return db.LeaveFederationResult{}, err
			}
			return db.LeaveFederationResult{}, credentialIOError(
				"delete managed reservation after leave",
			)
		}
	} else if result.ProjectUID != "" {
		if err := credentials.DeleteFederationCredential(ctx, result.ProjectUID); err != nil {
			if errors.Is(err, config.ErrFederationCredentialConflict) {
				return db.LeaveFederationResult{}, err
			}
			return db.LeaveFederationResult{}, credentialIOError(
				"delete federation replica credential after leave",
			)
		}
	}
	key := federationReplicaTransitionKey(store, project.Name)
	setFederationReplicaSuppressed(key, true)
	delete(federationReplicaLeaveIntents, key)
	return result, nil
}

// EnsureFederationReplicaResult describes the resulting local project and
// federation binding.
type EnsureFederationReplicaResult struct {
	Project               db.Project
	Binding               db.FederationBinding
	Adopted               bool
	AdoptionSnapshotCount int64
}

// EnsureFederationReplica creates or reuses a local spoke binding, optionally
// adopting an existing standalone project. Credentials are written only after
// the binding succeeds, push is enabled after credential persistence, and wake
// fires exactly once after every successful call.
func EnsureFederationReplica(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	wake func(),
	p EnsureFederationReplicaParams,
) (EnsureFederationReplicaResult, error) {
	normalized, err := normalizeFederationReplicaParams(p)
	if err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	p = normalized

	result, err := ensureFederationReplicaState(ctx, store, credentials, p)
	if err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	if wake != nil {
		wake()
	}
	return result, nil
}

// ReserveFederationReplicaCredential reserves an initial credential while
// holding the same local critical section as manual replica joins. The lock is
// released before the caller performs any hub network operation.
func ReserveFederationReplicaCredential(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	p ReserveFederationReplicaCredentialParams,
) error {
	p.ProjectName = strings.TrimSpace(p.ProjectName)
	if !katauid.Valid(p.HubProjectUID) || p.ProjectName == "" ||
		p.Credential.Token == "" || p.Credential.HubProjectID <= 0 {
		return federationReplicaError(
			ErrFederationReplicaInvalidInput,
			"hub_project_uid, project_name, and credential are required for reservation",
			"",
		)
	}
	if err := config.ValidateProjectName(p.ProjectName); err != nil {
		return federationReplicaError(ErrFederationReplicaInvalidInput, err.Error(), "")
	}
	if _, err := config.CanonicalHTTPOrigin(p.Credential.HubURL); err != nil {
		return federationReplicaError(
			ErrFederationReplicaInvalidInput,
			fmt.Sprintf("credential hub_url must be a valid HTTP(S) origin: %v", err),
			"",
		)
	}

	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return err
	}

	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()

	if err := prevalidateFederationReplicaReservation(ctx, store, p); err != nil {
		return err
	}
	if err := managed.ReserveManagedFederationCredential(
		ctx,
		config.FederationManagedCredentialReservation{
			ProjectUID: p.HubProjectUID,
			Credential: p.Credential,
		},
	); err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"federation credential reservation lost a concurrent update",
				"",
			)
		}
		return credentialIOError("reserve managed federation credential")
	}
	return nil
}

func prevalidateFederationReplicaReservation(
	ctx context.Context,
	store db.Storage,
	p ReserveFederationReplicaCredentialParams,
) error {
	project, err := store.ProjectByNameIncludingArchived(ctx, p.ProjectName)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("prevalidate federation credential reservation project: %w", err)
	}
	if project.DeletedAt != nil {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"local federation project changed before credential reservation",
			"",
		)
	}
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		if p.ExpectedBound {
			return federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"local federation binding disappeared before credential reservation",
				"",
			)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("prevalidate federation credential reservation binding: %w", err)
	}
	if !p.ExpectedBound {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"local federation binding appeared before credential reservation",
			"",
		)
	}
	expected := EnsureFederationReplicaParams{
		HubURL:        p.Credential.HubURL,
		HubProjectID:  p.Credential.HubProjectID,
		HubProjectUID: p.HubProjectUID,
		Credential:    p.Credential,
	}
	expected.Credential.Actor = strings.TrimSpace(binding.Actor)
	if details := replicaBindingConflictDetails(binding, expected); len(details) > 0 {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"local federation binding changed before credential reservation",
			"",
		)
	}
	return validateExistingReplicaCredentialCapabilities(binding, expected)
}

func ensureFederationReplicaState(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	p EnsureFederationReplicaParams,
) (EnsureFederationReplicaResult, error) {
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()

	if err := revalidateManagedReservation(ctx, credentials, p); err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	if err := rejectConflictingManagedReservation(ctx, credentials, p); err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	if err := ensureFederationReplicaCredentialTarget(ctx, store, credentials, p); err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	if err := ensureFederationReplicaCredentialRekey(ctx, store, credentials, p); err != nil {
		return EnsureFederationReplicaResult{}, err
	}

	result, err := ensureReplicaBindingOrAdopt(ctx, store, p)
	if err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	if p.Credential.Token != "" {
		if credentials == nil {
			return EnsureFederationReplicaResult{}, credentialIOError(
				"store federation replica credential",
			)
		}
		if err := credentials.StoreFederationCredential(ctx, result.Project.UID, p.Credential); err != nil {
			if errors.Is(err, config.ErrFederationCredentialConflict) {
				return EnsureFederationReplicaResult{}, err
			}
			return EnsureFederationReplicaResult{}, credentialIOError(
				"store federation replica credential",
			)
		}
	}
	if p.PushEnabled && !result.Binding.PushEnabled {
		result.Binding, err = enableReplicaPush(ctx, store, result.Project.ID)
		if err != nil {
			return EnsureFederationReplicaResult{}, err
		}
	}
	key := federationReplicaTransitionKey(store, p.ProjectName)
	setFederationReplicaSuppressed(key, false)
	delete(federationReplicaLeaveIntents, key)
	return result, nil
}

func revalidateManagedReservation(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	p EnsureFederationReplicaParams,
) error {
	if p.ManagedReservation == nil {
		return nil
	}
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return err
	}
	match, found, err := managed.FindManagedFederationCredential(ctx, p.ProjectName)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"managed federation reservations for the project disagree",
				"resolve the credential conflict, then retry reconciliation",
			)
		}
		return credentialIOError("read managed reservation")
	}
	if !found ||
		match.ProjectUID != p.ManagedReservation.ProjectUID ||
		match.Credential != p.ManagedReservation.Expected {
		return federationReplicaError(
			ErrFederationReplicaReservationChanged,
			"managed federation reservation changed while contacting the hub",
			"retry reconciliation or run kata federation leave for the project",
		)
	}
	return nil
}

func rejectConflictingManagedReservation(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	p EnsureFederationReplicaParams,
) error {
	finder, ok := credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return nil
	}
	match, found, err := finder.FindManagedFederationCredential(ctx, p.ProjectName)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"managed federation reservations for the project disagree",
				"",
			)
		}
		return credentialIOError("read managed federation reservation")
	}
	if !found {
		return nil
	}
	reservation := match.Credential
	reservationOrigin, reservationOriginErr := config.CanonicalHTTPOrigin(reservation.HubURL)
	requestedOrigin, requestedOriginErr := config.CanonicalHTTPOrigin(p.HubURL)
	if reservationOriginErr != nil ||
		requestedOriginErr != nil ||
		reservationOrigin != requestedOrigin ||
		reservation.HubProjectID != p.HubProjectID ||
		reservation.Token != p.Credential.Token {
		return federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"managed federation reservation conflicts with requested adoption",
			"",
		)
	}
	return nil
}

func validateExistingReplicaCredentialCapabilities(
	binding db.FederationBinding,
	p EnsureFederationReplicaParams,
) error {
	if binding.PushEnabled && p.Credential.Token != "" &&
		!federationCapabilitiesContain(p.Credential.Capabilities, "push") {
		return federationReplicaError(
			errFederationReplicaCapabilityMismatch,
			"push-enabled federation replica credentials require push capability",
			"",
		)
	}
	return nil
}

func normalizeFederationReplicaParams(
	p EnsureFederationReplicaParams,
) (EnsureFederationReplicaParams, error) {
	p.ProjectName = strings.TrimSpace(p.ProjectName)
	if strings.TrimSpace(p.HubURL) == "" || p.HubProjectID <= 0 || p.HubProjectUID == "" ||
		p.ProjectName == "" || p.ReplayHorizonEventID <= 0 {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput,
			"hub_url, hub_project_id, hub_project_uid, project_name, and replay_horizon_event_id are required",
			"",
		)
	}
	hubBaseURL, err := normalizeFederationHubBaseURL(p.HubURL)
	if err != nil {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput,
			err.Error(),
			"",
		)
	}
	p.HubURL = hubBaseURL
	if !katauid.Valid(p.HubProjectUID) {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, "hub_project_uid must be a valid UID", "",
		)
	}
	if err := config.ValidateProjectName(p.ProjectName); err != nil {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, err.Error(), "",
		)
	}

	p.Credential.Actor = strings.TrimSpace(p.Credential.Actor)
	if err := db.ValidateTokenActor(p.Credential.Actor); err != nil {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, err.Error(), "",
		)
	}
	capabilities, err := normalizedReplicaCapabilities(p.Credential.Capabilities)
	if err != nil {
		return EnsureFederationReplicaParams{}, err
	}
	p.Credential.Capabilities = capabilities
	if p.PushEnabled && !federationCapabilitiesContain(capabilities, "push") {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			errFederationReplicaCapabilityMismatch,
			"push-enabled federation replica requires push capability",
			"",
		)
	}
	if p.AdoptExisting {
		if !p.PushEnabled {
			return EnsureFederationReplicaParams{}, federationReplicaError(
				errFederationReplicaCapabilityMismatch,
				"adopting an existing project requires push to be enabled",
				"",
			)
		}
		if !federationCapabilitiesContain(capabilities, "pull") ||
			!federationCapabilitiesContain(capabilities, "push") {
			return EnsureFederationReplicaParams{}, federationReplicaError(
				errFederationReplicaCapabilityMismatch,
				"adopting an existing project requires pull and push capabilities",
				"",
			)
		}
	}
	if p.CredentialRekey != nil {
		if !p.AdoptExisting ||
			!katauid.Valid(p.CredentialRekey.ProjectUID) ||
			p.CredentialRekey.ProjectUID == p.HubProjectUID {
			return EnsureFederationReplicaParams{}, federationReplicaError(
				ErrFederationReplicaInvalidInput,
				"credential rekey requires a distinct valid adoption source project UID",
				"",
			)
		}
	}
	if p.Credential.Token != "" {
		credentialBaseURL, err := normalizeFederationHubBaseURL(p.Credential.HubURL)
		if err != nil {
			return EnsureFederationReplicaParams{}, federationReplicaError(
				ErrFederationReplicaInvalidInput,
				err.Error(),
				"",
			)
		}
		if credentialBaseURL != hubBaseURL || p.Credential.HubProjectID != p.HubProjectID {
			return EnsureFederationReplicaParams{}, federationReplicaError(
				ErrFederationReplicaInvalidInput,
				"credential hub target must match the requested federation hub",
				"",
			)
		}
		p.Credential.HubURL = credentialBaseURL
	}
	effectiveAllowInsecure, err := config.EffectiveHTTPAllowInsecure(
		p.HubURL, p.Credential.AllowInsecure,
	)
	if err != nil {
		return EnsureFederationReplicaParams{}, federationReplicaError(
			ErrFederationReplicaInvalidInput,
			"hub_url must be a valid HTTP(S) base URL",
			"",
		)
	}
	p.Credential.AllowInsecure = effectiveAllowInsecure
	return p, nil
}

func normalizeFederationHubBaseURL(raw string) (string, error) {
	baseURL, err := config.CanonicalHTTPBaseURL(raw)
	if err != nil {
		return "", errors.New(
			"hub_url must be an HTTP(S) base URL without user info, query, or fragment",
		)
	}
	return baseURL, nil
}

func normalizedReplicaCapabilities(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	capabilities, err := db.CanonicalFederationCapabilities(raw)
	if err != nil {
		return "", federationReplicaError(ErrFederationReplicaInvalidInput, err.Error(), "")
	}
	return capabilities, nil
}

func federationCapabilitiesContain(capabilities, want string) bool {
	for _, part := range strings.Split(capabilities, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func ensureFederationReplicaCredentialTarget(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	p EnsureFederationReplicaParams,
) error {
	if credentials == nil {
		if p.Credential.Token == "" {
			return nil
		}
		return credentialIOError("read federation replica credential")
	}
	existing, ok, err := credentials.FederationCredential(ctx, p.HubProjectUID)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return err
		}
		return credentialIOError("read federation replica credential")
	}
	if !ok {
		return nil
	}
	existingBaseURL, err := config.CanonicalHTTPBaseURL(existing.HubURL)
	if err != nil {
		return federationReplicaCredentialTargetConflict(
			ctx, store, p,
			"existing federation credential has an invalid hub_url",
		)
	}
	requestedBaseURL, _ := config.CanonicalHTTPBaseURL(p.Credential.HubURL)
	existingAllowInsecure, existingPolicyErr := config.EffectiveHTTPAllowInsecure(
		existing.HubURL, existing.AllowInsecure,
	)
	requestedAllowInsecure, requestedPolicyErr := config.EffectiveHTTPAllowInsecure(
		p.Credential.HubURL, p.Credential.AllowInsecure,
	)
	if existingBaseURL != requestedBaseURL || existingPolicyErr != nil || requestedPolicyErr != nil ||
		existingAllowInsecure != requestedAllowInsecure {
		return federationReplicaCredentialTargetConflict(
			ctx, store, p,
			"existing federation credential targets another hub endpoint",
		)
	}
	if existing.HubProjectID != p.Credential.HubProjectID ||
		(p.Credential.Token != "" && existing.ManagedByConfig && existing.Token != p.Credential.Token) {
		return federationReplicaCredentialTargetConflict(
			ctx, store, p,
			"existing federation credential differs from the requested hub",
		)
	}
	return nil
}

func federationReplicaCredentialTargetConflict(
	ctx context.Context,
	store db.Storage,
	p EnsureFederationReplicaParams,
	credentialMessage string,
) error {
	project, err := store.ProjectByUID(ctx, p.HubProjectUID)
	if err == nil {
		binding, bindingErr := store.FederationBindingByProject(ctx, project.ID)
		if bindingErr == nil {
			if details := replicaBindingConflictDetails(binding, p); len(details) > 0 {
				return federationReplicaError(
					ErrFederationReplicaBindingConflict,
					"existing federation binding differs from the requested hub: "+
						strings.Join(details, ", "),
					"",
				)
			}
		} else if !errors.Is(bindingErr, db.ErrNotFound) {
			return fmt.Errorf(
				"read existing federation binding after credential conflict: %w",
				bindingErr,
			)
		}
	} else if !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf(
			"read existing federation project after credential conflict: %w",
			err,
		)
	}
	return federationReplicaError(
		ErrFederationReplicaCredentialConflict,
		credentialMessage,
		"",
	)
}

func ensureFederationReplicaCredentialRekey(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	p EnsureFederationReplicaParams,
) error {
	source := p.CredentialRekey
	if source == nil && p.AdoptExisting {
		project, err := store.ProjectByNameIncludingArchived(ctx, p.ProjectName)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return fmt.Errorf("resolve federation credential adoption source: %w", err)
		}
		if err == nil && project.UID != p.HubProjectUID {
			if credentials == nil {
				return credentialIOError("read federation credential adoption source")
			}
			existing, ok, readErr := credentials.FederationCredential(ctx, project.UID)
			if readErr != nil {
				if errors.Is(readErr, config.ErrFederationCredentialConflict) {
					return readErr
				}
				return credentialIOError("read federation credential adoption source")
			}
			if ok {
				if existing != p.Credential {
					return federationReplicaError(
						ErrFederationReplicaCredentialConflict,
						"standalone project credential differs from the requested credential",
						"",
					)
				}
				source = &FederationReplicaCredentialRekeySource{
					ProjectUID: project.UID,
					Expected:   existing,
				}
			}
		}
	}
	if source == nil {
		return nil
	}
	sourceProject, err := store.ProjectByUID(ctx, source.ProjectUID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("resolve federation credential rekey source: %w", err)
	}
	if err == nil {
		if _, bindingErr := store.FederationBindingByProject(ctx, sourceProject.ID); bindingErr == nil {
			return federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"federation credential adoption source is already bound",
				"",
			)
		} else if !errors.Is(bindingErr, db.ErrNotFound) {
			return fmt.Errorf("read federation credential rekey source binding: %w", bindingErr)
		}
	}
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return err
	}
	if err := managed.RekeyFederationCredential(ctx, config.FederationCredentialRekey{
		FromProjectUID: source.ProjectUID,
		ToProjectUID:   p.HubProjectUID,
		Expected:       source.Expected,
		Replacement:    p.Credential,
	}); err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"federation credential rekey lost a concurrent update",
				"",
			)
		}
		return credentialIOError("rekey federation replica credential")
	}
	return nil
}

func managedCredentialStore(
	credentials config.FederationCredentialStore,
) (config.FederationManagedCredentialStore, error) {
	managed, ok := credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return nil, fmt.Errorf(
			"%w: credential store lacks managed federation operations",
			ErrFederationReplicaCredentialIO,
		)
	}
	return managed, nil
}

func credentialIOError(operation string) error {
	return fmt.Errorf("%w: %s", ErrFederationReplicaCredentialIO, operation)
}

func ensureReplicaBindingOrAdopt(
	ctx context.Context,
	store db.Storage,
	p EnsureFederationReplicaParams,
) (EnsureFederationReplicaResult, error) {
	if p.AdoptExisting {
		if result, adopted, err := adoptExistingReplica(ctx, store, p); err != nil {
			return EnsureFederationReplicaResult{}, err
		} else if adopted {
			return EnsureFederationReplicaResult{
				Project:               result.Project,
				Binding:               result.Binding,
				Adopted:               true,
				AdoptionSnapshotCount: result.AdoptionSnapshotCount,
			}, nil
		}
	}
	project, binding, err := ensureReplicaBinding(ctx, store, p)
	if err != nil {
		return EnsureFederationReplicaResult{}, err
	}
	return EnsureFederationReplicaResult{Project: project, Binding: binding}, nil
}

func adoptExistingReplica(
	ctx context.Context,
	store db.Storage,
	p EnsureFederationReplicaParams,
) (db.AdoptProjectIntoFederationResult, bool, error) {
	projectName := p.ProjectName
	if project, err := store.ProjectByUID(ctx, p.HubProjectUID); err == nil {
		if project.DeletedAt != nil {
			return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
				errFederationReplicaProjectCollision,
				"hub project UID belongs to an archived local project",
				"",
			)
		}
		binding, bindErr := store.FederationBindingByProject(ctx, project.ID)
		if bindErr != nil {
			if errors.Is(bindErr, db.ErrNotFound) {
				if project.Name != projectName {
					return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
						errFederationReplicaProjectCollision,
						fmt.Sprintf(
							"hub project UID belongs to local project %q; cannot adopt local project %q",
							project.Name, projectName,
						),
						"",
					)
				}
				// An explicit adoption on an unbound UID-holder is the
				// actor-safe transmission path for existing local events.
				result, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
					ProjectID:            project.ID,
					HubURL:               p.HubURL,
					HubProjectID:         p.HubProjectID,
					HubProjectUID:        p.HubProjectUID,
					ReplayHorizonEventID: p.ReplayHorizonEventID,
					Actor:                p.Credential.Actor,
					AllowInsecure:        p.Credential.AllowInsecure,
				})
				if err != nil {
					if errors.Is(err, db.ErrIssueSyncFederationBinding) {
						return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
							errFederationReplicaIssueSyncConflict,
							"project has issue sync enabled; run GitHub sync on the federation hub, or disable issue sync before joining this project as a spoke",
							"",
						)
					}
					return db.AdoptProjectIntoFederationResult{}, false,
						fmt.Errorf("adopt existing federation replica: %w", err)
				}
				return result, true, nil
			}
			return db.AdoptProjectIntoFederationResult{}, false,
				fmt.Errorf("read existing federation binding: %w", bindErr)
		}
		if details := replicaBindingConflictDetails(binding, p); len(details) > 0 {
			return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"existing federation binding differs from the requested hub: "+strings.Join(details, ", "),
				"",
			)
		}
		if project.Name != projectName {
			return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
				errFederationReplicaProjectCollision,
				fmt.Sprintf(
					"hub project UID is already bound to local project %q; cannot adopt local project %q",
					project.Name, projectName,
				),
				"",
			)
		}
		return db.AdoptProjectIntoFederationResult{}, false, nil
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, false,
			fmt.Errorf("lookup federation project UID: %w", err)
	}
	existing, err := store.ProjectByNameIncludingArchived(ctx, projectName)
	if errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
			errFederationReplicaProjectNotFound,
			"adoption requested but no local project exists with this name",
			"",
		)
	}
	if err != nil {
		return db.AdoptProjectIntoFederationResult{}, false,
			fmt.Errorf("lookup federation project name: %w", err)
	}
	if existing.UID == p.HubProjectUID {
		return db.AdoptProjectIntoFederationResult{}, false, federationReplicaError(
			errFederationReplicaProjectCollision,
			"hub project UID already exists locally but is not bound to federation",
			"",
		)
	}
	if existing.DeletedAt != nil {
		return db.AdoptProjectIntoFederationResult{}, true, federationReplicaError(
			errFederationReplicaProjectCollision,
			"a deleted project with this name cannot be adopted into federation",
			"",
		)
	}
	if binding, err := store.FederationBindingByProject(ctx, existing.ID); err == nil {
		return db.AdoptProjectIntoFederationResult{}, true, federationReplicaError(
			ErrFederationReplicaBindingConflict,
			fmt.Sprintf("project already has %q federation binding", binding.Role),
			"",
		)
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.AdoptProjectIntoFederationResult{}, true,
			fmt.Errorf("read adoption target federation binding: %w", err)
	}
	result, err := store.AdoptProjectIntoFederation(ctx, db.AdoptProjectIntoFederationParams{
		ProjectID:            existing.ID,
		HubURL:               p.HubURL,
		HubProjectID:         p.HubProjectID,
		HubProjectUID:        p.HubProjectUID,
		ReplayHorizonEventID: p.ReplayHorizonEventID,
		Actor:                p.Credential.Actor,
		AllowInsecure:        p.Credential.AllowInsecure,
	})
	if err != nil {
		if errors.Is(err, db.ErrIssueSyncFederationBinding) {
			return db.AdoptProjectIntoFederationResult{}, true, federationReplicaError(
				errFederationReplicaIssueSyncConflict,
				"project has issue sync enabled; run GitHub sync on the federation hub, or disable issue sync before joining this project as a spoke",
				"",
			)
		}
		return db.AdoptProjectIntoFederationResult{}, true,
			fmt.Errorf("adopt federation replica: %w", err)
	}
	return result, true, nil
}

func ensureReplicaBinding(
	ctx context.Context,
	store db.Storage,
	p EnsureFederationReplicaParams,
) (db.Project, db.FederationBinding, error) {
	projectName := p.ProjectName
	project, err := store.ProjectByUID(ctx, p.HubProjectUID)
	createdProject := false
	if errors.Is(err, db.ErrNotFound) {
		if existing, lookupErr := store.ProjectByNameIncludingArchived(ctx, projectName); lookupErr == nil {
			if existing.UID != p.HubProjectUID {
				return db.Project{}, db.FederationBinding{}, federationReplicaError(
					errFederationReplicaProjectCollision,
					"a project with this name already has a different UID; rerun with --adopt-existing --push to adopt it into federation",
					"",
				)
			}
		} else if !errors.Is(lookupErr, db.ErrNotFound) {
			return db.Project{}, db.FederationBinding{},
				fmt.Errorf("lookup federation project name: %w", lookupErr)
		}
		project, err = store.CreateProjectWithUID(ctx, projectName, p.HubProjectUID)
		if err != nil {
			return db.Project{}, db.FederationBinding{},
				fmt.Errorf("create federation replica project: %w", err)
		}
		createdProject = true
	} else if err != nil {
		return db.Project{}, db.FederationBinding{},
			fmt.Errorf("lookup federation project UID: %w", err)
	} else if project.DeletedAt != nil {
		return db.Project{}, db.FederationBinding{}, federationReplicaError(
			errFederationReplicaProjectCollision,
			fmt.Sprintf(
				"an archived local project %q already has the hub project UID; restore it with `kata projects restore` first",
				project.Name,
			),
			"",
		)
	}

	replayHorizon := p.ReplayHorizonEventID
	cursor := replayHorizon - 1
	if cursor < 0 {
		cursor = 0
	}
	pushEnabled := false
	pushCursor := int64(0)
	existing, err := store.FederationBindingByProject(ctx, project.ID)
	if err == nil {
		if details := replicaBindingConflictDetails(existing, p); len(details) > 0 {
			return db.Project{}, db.FederationBinding{}, federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"existing federation binding differs from the requested hub: "+strings.Join(details, ", "),
				"",
			)
		}
		if err := validateExistingReplicaCredentialCapabilities(existing, p); err != nil {
			return db.Project{}, db.FederationBinding{}, err
		}
		replayHorizon = existing.ReplayHorizonEventID
		cursor = existing.PullCursorEventID
		pushEnabled = existing.PushEnabled
		pushCursor = existing.PushCursorEventID
	} else if !errors.Is(err, db.ErrNotFound) {
		return db.Project{}, db.FederationBinding{},
			fmt.Errorf("read federation replica binding: %w", err)
	} else if !createdProject {
		// An unbound local project holding the hub project UID is the normal
		// post-leave state: leave removes the binding but the project keeps the
		// shared identity. A join naming that project is a rejoin — rebind it.
		// Pull restarts just below the replay horizon (event-UID dedup absorbs
		// the overlap) and a push-enabled rejoin re-offers local-origin events
		// from cursor 0 so the hub dedups what it already has and absorbs edits
		// made while the project was standalone.
		//
		// Trust model: a spoke and the hubs it federates with trust each other
		// (docs/design/federation.md "Tokens And Trust Boundaries" / "No
		// Multi-Tenant Authorization Model"). The UID is an unguessable ULID, so
		// a hub reporting it as a project identity means it IS the project that
		// federated there; we do not defend against a hostile hub forging a known
		// UID to capture local data (out of scope). The operator-facing rejoin
		// preview (CLI/TUI) is the confirmation surface.
		if project.Name != projectName {
			return db.Project{}, db.FederationBinding{}, federationReplicaError(
				errFederationReplicaRejoinNameMismatch,
				fmt.Sprintf(
					"hub project UID is held by local project %q, which previously left this federation; rerun join with --project %q to rejoin it",
					project.Name, project.Name,
				),
				"",
			)
		}
		if p.PushEnabled {
			pushEnabled = true
		}
	}

	binding, err := store.UpsertFederationBinding(ctx, db.FederationBinding{
		ProjectID:            project.ID,
		Role:                 db.FederationRoleSpoke,
		HubURL:               p.HubURL,
		HubProjectID:         p.HubProjectID,
		HubProjectUID:        p.HubProjectUID,
		ReplayHorizonEventID: replayHorizon,
		PullCursorEventID:    cursor,
		PushEnabled:          pushEnabled,
		PushCursorEventID:    pushCursor,
		Actor:                p.Credential.Actor,
		AllowInsecure:        p.Credential.AllowInsecure,
		Enabled:              true,
	})
	if err != nil {
		if errors.Is(err, db.ErrIssueSyncFederationBinding) {
			return db.Project{}, db.FederationBinding{}, federationReplicaError(
				errFederationReplicaIssueSyncConflict,
				"project has issue sync enabled; run GitHub sync on the federation hub, or disable issue sync before joining this project as a spoke",
				"",
			)
		}
		return db.Project{}, db.FederationBinding{},
			fmt.Errorf("upsert federation replica binding: %w", err)
	}
	return project, binding, nil
}

func enableReplicaPush(
	ctx context.Context, store db.Storage, projectID int64,
) (db.FederationBinding, error) {
	localCursor, err := store.MaxLocalOriginEventID(ctx, projectID)
	if err != nil {
		return db.FederationBinding{}, fmt.Errorf("read local federation push cursor: %w", err)
	}
	binding, err := store.EnableFederationPush(ctx, projectID, localCursor)
	if err != nil {
		return db.FederationBinding{}, fmt.Errorf("enable federation replica push: %w", err)
	}
	return binding, nil
}

func replicaBindingConflictDetails(
	existing db.FederationBinding, p EnsureFederationReplicaParams,
) []string {
	existingActor := strings.TrimSpace(existing.Actor)
	requestedActor := p.Credential.Actor
	actorCompatible := existingActor == requestedActor || (existingActor == "" && requestedActor != "")
	details := make([]string, 0, 5)
	if existing.Role != db.FederationRoleSpoke {
		details = append(details, fmt.Sprintf(
			"role existing=%s requested=%s", existing.Role, db.FederationRoleSpoke,
		))
	}
	existingBaseURL, existingBaseURLErr := config.CanonicalHTTPBaseURL(existing.HubURL)
	requestedBaseURL, requestedBaseURLErr := config.CanonicalHTTPBaseURL(p.HubURL)
	switch {
	case existingBaseURLErr != nil:
		details = append(details, fmt.Sprintf("hub_url existing=%s invalid=%v", existing.HubURL, existingBaseURLErr))
	case requestedBaseURLErr != nil:
		details = append(details, fmt.Sprintf("hub_url requested=%s invalid=%v", p.HubURL, requestedBaseURLErr))
	case existingBaseURL != requestedBaseURL:
		details = append(details, fmt.Sprintf(
			"hub_url existing=%s requested=%s", existing.HubURL, p.HubURL,
		))
	}
	existingAllowInsecure, existingPolicyErr := config.EffectiveHTTPAllowInsecure(
		existing.HubURL, existing.AllowInsecure,
	)
	requestedAllowInsecure, requestedPolicyErr := config.EffectiveHTTPAllowInsecure(
		p.HubURL, p.Credential.AllowInsecure,
	)
	if existingPolicyErr == nil && requestedPolicyErr == nil &&
		existingAllowInsecure != requestedAllowInsecure {
		details = append(details, fmt.Sprintf(
			"allow_insecure existing=%t requested=%t",
			existingAllowInsecure, requestedAllowInsecure,
		))
	}
	if existing.HubProjectID != p.HubProjectID {
		details = append(details, fmt.Sprintf(
			"hub_project_id existing=%d requested=%d", existing.HubProjectID, p.HubProjectID,
		))
	}
	if existing.HubProjectUID != p.HubProjectUID {
		details = append(details, fmt.Sprintf(
			"hub_project_uid existing=%s requested=%s", existing.HubProjectUID, p.HubProjectUID,
		))
	}
	if !actorCompatible {
		details = append(details, fmt.Sprintf(
			"actor existing=%s requested=%s", existingActor, requestedActor,
		))
	}
	return details
}

func federationReplicaError(kind error, message, hint string) *FederationReplicaError {
	return &FederationReplicaError{kind: kind, message: message, hint: hint}
}
