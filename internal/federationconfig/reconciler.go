package federationconfig

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federation"
	katauid "go.kenn.io/kata/internal/uid"
)

const configCapabilities = "pull,push,lease"

const (
	initialRetryDelay = time.Second
	maxRetryDelay     = 5 * time.Minute
)

var (
	// ErrConfigurationConflict classifies an incompatible managed or manual credential.
	ErrConfigurationConflict = errors.New("federation configuration conflict")
	// ErrBindingConflict classifies an incompatible existing local binding.
	ErrBindingConflict = errors.New("federation binding conflict")
	// ErrCredentialIO classifies failures reading or persisting local credentials.
	ErrCredentialIO = errors.New("federation credential I/O")
	// ErrLocalStorage classifies local project and binding storage failures.
	ErrLocalStorage = errors.New("federation local storage")
)

type reconciliationError struct {
	kind    error
	message string
}

func (e *reconciliationError) Error() string { return e.message }
func (e *reconciliationError) Unwrap() error { return e.kind }

// Timer is the cancellable timer contract used by the reconciler scheduler.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock supplies process-local time and timers to the reconciler scheduler.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Target binds one normalized project mapping to its selected daemon catalog
// entry. Authentication remains scoped to that one catalog entry.
type Target struct {
	Catalog config.CatalogDaemonConfig
	Mapping config.FederationProjectConfig
}

// HubFactory constructs an origin-pinned hub client for one attempt.
type HubFactory func(context.Context, config.CatalogDaemonConfig) (Hub, error)

// ReconcilerConfig contains the process-local controller dependencies.
type ReconcilerConfig struct {
	Store       db.Storage
	Credentials config.FederationCredentialStore
	Targets     []Target
	HubFactory  HubFactory
	Clock       Clock
	Wake        func()
	Logger      *log.Logger
}

// Health is the sanitized aggregate state of configured federation mappings.
type Health struct {
	Configured        int
	Reconciled        int
	Pending           int
	Conflicted        int
	LastAttemptAt     *time.Time
	LastSuccessAt     *time.Time
	LastErrorCategory string
	LastErrorStatus   int
}

type reconciliationState struct {
	state             string
	nextAttempt       time.Time
	nextDelay         time.Duration
	lastAttempt       *time.Time
	lastSuccess       *time.Time
	lastErrorCategory string
	lastErrorStatus   int
}

// Reconciler serially reconciles due mappings while maintaining independent
// retry state for each mapping.
type Reconciler struct {
	store       db.Storage
	credentials config.FederationCredentialStore
	targets     []Target
	hubFactory  HubFactory
	clock       Clock
	wake        func()
	logger      *log.Logger

	mu     sync.Mutex
	states []reconciliationState
}

// NewReconciler constructs a process-local federation configuration
// reconciler. Run owns scheduling; Health may be called concurrently.
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	clock := cfg.Clock
	if clock == nil {
		clock = wallClock{}
	}
	targets := append([]Target(nil), cfg.Targets...)
	return &Reconciler{
		store:       cfg.Store,
		credentials: cfg.Credentials,
		targets:     targets,
		hubFactory:  cfg.HubFactory,
		clock:       clock,
		wake:        cfg.Wake,
		logger:      cfg.Logger,
		states:      make([]reconciliationState, len(targets)),
	}
}

// Run attempts due mappings in configuration order until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	for {
		now := r.clock.Now()
		attempted := false
		for i := range r.targets {
			if !r.due(i, now) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			attempted = true
			attemptAt := r.clock.Now()
			r.markAttemptStarted(i, attemptAt)
			err := r.reconcile(ctx, r.targets[i])
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			r.recordAttempt(i, attemptAt, err)
		}
		if attempted {
			continue
		}

		next, ok := r.nextDue()
		if !ok {
			<-ctx.Done()
			return ctx.Err()
		}
		delay := next.Sub(r.clock.Now())
		if delay <= 0 {
			continue
		}
		timer := r.clock.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C():
			timer.Stop()
		}
	}
}

// Health returns a mutex-safe, coordinate-free aggregate snapshot.
func (r *Reconciler) Health() Health {
	r.mu.Lock()
	defer r.mu.Unlock()

	health := Health{Configured: len(r.states)}
	var lastErrorAt time.Time
	for i := range r.states {
		state := &r.states[i]
		switch state.state {
		case "reconciled":
			health.Reconciled++
		case "conflict":
			health.Conflicted++
		default:
			health.Pending++
		}
		health.LastAttemptAt = laterTime(health.LastAttemptAt, state.lastAttempt)
		health.LastSuccessAt = laterTime(health.LastSuccessAt, state.lastSuccess)
		if state.lastErrorCategory != "" && state.lastAttempt != nil &&
			(lastErrorAt.IsZero() || !state.lastAttempt.Before(lastErrorAt)) {
			lastErrorAt = *state.lastAttempt
			health.LastErrorCategory = state.lastErrorCategory
			health.LastErrorStatus = state.lastErrorStatus
		}
	}
	return health
}

func (r *Reconciler) reconcile(ctx context.Context, target Target) error {
	if r.hubFactory == nil {
		return reconcileError(ErrConfigurationConflict, "missing federation hub factory")
	}
	hub, err := r.hubFactory(ctx, target.Catalog)
	if err != nil {
		return err
	}
	return ReconcileMapping(
		ctx, r.store, r.credentials, hub, target.Catalog, target.Mapping, r.wake,
	)
}

func (r *Reconciler) due(index int, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.states[index]
	return state.state != "reconciled" &&
		(state.nextAttempt.IsZero() || !state.nextAttempt.After(now))
}

func (r *Reconciler) nextDue() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var next time.Time
	for i := range r.states {
		state := &r.states[i]
		if state.state == "reconciled" {
			continue
		}
		if next.IsZero() || state.nextAttempt.Before(next) {
			next = state.nextAttempt
		}
	}
	return next, !next.IsZero()
}

func (r *Reconciler) markAttemptStarted(index int, attemptAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states[index].lastAttempt = timePointer(attemptAt)
}

func (r *Reconciler) recordAttempt(index int, attemptAt time.Time, err error) {
	category, status := classifyReconciliationError(err)
	stateName := "reconciled"
	completedAt := r.clock.Now()
	if err != nil {
		stateName = "pending"
		if category == "configuration_conflict" || category == "binding_conflict" {
			stateName = "conflict"
		}
	}

	r.mu.Lock()
	state := &r.states[index]
	catalogName := r.targets[index].Catalog.Name
	spokeProject := r.targets[index].Mapping.SpokeProject
	hubProject := r.targets[index].Mapping.HubProject
	transitioned := state.state != stateName ||
		state.lastErrorCategory != category ||
		state.lastErrorStatus != status
	state.state = stateName
	state.lastAttempt = timePointer(attemptAt)
	state.lastErrorCategory = category
	state.lastErrorStatus = status
	if err == nil {
		state.lastSuccess = timePointer(completedAt)
		state.nextAttempt = time.Time{}
		state.nextDelay = initialRetryDelay
	} else {
		delay := state.nextDelay
		if delay == 0 {
			delay = initialRetryDelay
		}
		state.nextAttempt = completedAt.Add(delay)
		state.nextDelay = doubledDelay(delay)
	}
	r.mu.Unlock()

	if transitioned && r.logger != nil {
		r.logger.Printf(
			"federation config reconciliation hub=%s spoke_project=%s hub_project=%s state=%s category=%s status=%d",
			catalogName, spokeProject, hubProject, stateName, category, status,
		)
	}
}

func classifyReconciliationError(err error) (string, int) {
	if err == nil {
		return "", 0
	}
	var hubErr *HubError
	status := 0
	if errors.As(err, &hubErr) && hubErr != nil {
		status = hubErr.StatusCode
	}
	switch {
	case errors.Is(err, ErrConfigurationConflict):
		return "configuration_conflict", status
	case errors.Is(err, ErrBindingConflict):
		return "binding_conflict", status
	case errors.Is(err, ErrCredentialIO):
		return "credential_io", status
	case errors.Is(err, ErrLocalStorage):
		return "local_storage", status
	case errors.Is(err, ErrHubUnavailable):
		return "hub_unavailable", status
	case errors.Is(err, ErrHubAuthentication):
		return "hub_authentication", status
	case errors.Is(err, ErrHubValidation):
		return "hub_validation", status
	default:
		return "internal", status
	}
}

func laterTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		return timePointer(*candidate)
	}
	return current
}

func doubledDelay(delay time.Duration) time.Duration {
	if delay >= maxRetryDelay/2 {
		return maxRetryDelay
	}
	return min(delay*2, maxRetryDelay)
}

func timePointer(value time.Time) *time.Time {
	return &value
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

func (wallClock) NewTimer(delay time.Duration) Timer {
	return wallTimer{timer: time.NewTimer(delay)}
}

type wallTimer struct {
	timer *time.Timer
}

func (t wallTimer) C() <-chan time.Time { return t.timer.C }
func (t wallTimer) Stop() bool          { return t.timer.Stop() }

// ReconcileMapping performs one restart-safe attempt for a single normalized
// mapping. Scheduling, retry, and health aggregation are layered on this
// operation by the process controller.
func ReconcileMapping(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	hub Hub,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
	wake func(),
) error {
	managed, ok := credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return reconcileError(
			ErrCredentialIO,
			"credential store does not support managed federation operations",
		)
	}
	if daemon.FederationReplicaMappingSuppressed(store, mapping.SpokeProject) {
		return nil
	}
	if handled, err := reconcilePendingLeave(
		ctx, store, managed, hub, catalog, mapping,
	); handled {
		return err
	}
	preflight, err := preflightMapping(
		ctx, store, credentials, managed, hub, catalog, mapping,
	)
	if err != nil {
		return err
	}
	endHubOperation, err := daemon.BeginFederationReplicaHubOperation(
		ctx,
		store,
		credentials,
		mapping.SpokeProject,
		config.FederationManagedCredentialReservation{
			ProjectUID: preflight.credential.key,
			Credential: preflight.credential.credential,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, daemon.ErrFederationReplicaLeavePending):
			return nil
		case errors.Is(err, daemon.ErrFederationReplicaCredentialIO):
			return reconcileError(ErrCredentialIO, "read federation credential before enrollment")
		default:
			return reconcileError(
				ErrConfigurationConflict,
				"federation credential changed before enrollment",
			)
		}
	}
	enrollment, err := ensureMappingEnrollment(
		ctx, store, hub, catalog, mapping, preflight,
	)
	enrollmentID := int64(0)
	if err == nil {
		enrollmentID = enrollment.id
	}
	leavePending, finishErr := endHubOperation(ctx, enrollmentID)
	if finishErr != nil {
		return reconcileError(ErrCredentialIO, "finish federation hub operation")
	}
	if leavePending {
		if err != nil {
			return nil
		}
		if revokeErr := hub.RevokeEnrollment(ctx, enrollment.id); revokeErr != nil {
			return safeHubError(revokeErr)
		}
		if confirmErr := confirmPendingEnrollmentCleanup(
			ctx, managed, mapping.SpokeProject, enrollment.id,
		); confirmErr != nil {
			return confirmErr
		}
		return nil
	}
	if err != nil {
		return err
	}
	reservationChanged, err := convergeLocalMapping(
		ctx, store, credentials, wake, mapping, preflight, enrollment.credential,
	)
	if err == nil || enrollment.id == 0 || !reservationChanged {
		return err
	}
	pending, pendingFound, pendingErr := managed.FindManagedFederationCredential(
		ctx, mapping.SpokeProject,
	)
	if pendingErr != nil {
		return reconcileError(ErrCredentialIO, "read pending federation leave")
	}
	if pendingFound && pending.Credential.LeavePending {
		replacement := pending
		replacement.Credential.PendingEnrollmentID = enrollment.id
		if replaceErr := managed.ReplaceManagedFederationCredential(
			ctx, pending, replacement,
		); replaceErr != nil {
			return reconcileError(ErrCredentialIO, "record pending enrollment cleanup")
		}
		pending = replacement
	}
	if daemon.FederationReplicaMappingSuppressed(store, mapping.SpokeProject) &&
		!pendingFound {
		// A completed local-only leave deliberately skips hub cleanup. The
		// process-local suppression still prevents this mapping from being
		// recreated until restart.
		return nil
	}
	if revokeErr := hub.RevokeEnrollment(ctx, enrollment.id); revokeErr != nil {
		if daemon.FederationReplicaMappingSuppressed(store, mapping.SpokeProject) {
			return nil
		}
		return safeHubError(revokeErr)
	}
	if pendingFound && pending.Credential.LeavePending {
		if confirmErr := confirmPendingEnrollmentCleanup(
			ctx, managed, mapping.SpokeProject, enrollment.id,
		); confirmErr != nil &&
			!daemon.FederationReplicaMappingSuppressed(store, mapping.SpokeProject) {
			return confirmErr
		}
	}
	return nil
}

func confirmPendingEnrollmentCleanup(
	ctx context.Context,
	managed config.FederationManagedCredentialStore,
	projectName string,
	enrollmentID int64,
) error {
	pending, found, err := managed.FindManagedFederationCredential(ctx, projectName)
	if err != nil {
		return reconcileError(ErrCredentialIO, "read pending enrollment cleanup")
	}
	if !found || !pending.Credential.LeavePending ||
		pending.Credential.PendingEnrollmentID == 0 {
		return nil
	}
	if pending.Credential.PendingEnrollmentID != enrollmentID {
		return reconcileError(
			ErrConfigurationConflict,
			"pending enrollment cleanup changed during revocation",
		)
	}
	return nil
}

func reconcilePendingLeave(
	ctx context.Context,
	store db.Storage,
	managed config.FederationManagedCredentialStore,
	hub Hub,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) (bool, error) {
	pending, found, err := managed.FindManagedFederationCredential(ctx, mapping.SpokeProject)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return true, reconcileError(
				ErrConfigurationConflict,
				"managed federation reservations disagree during leave",
			)
		}
		return true, reconcileError(ErrCredentialIO, "read pending federation leave")
	}
	if !found || !pending.Credential.LeavePending {
		return false, nil
	}
	if !credentialManagementMatches(pending.Credential, catalog, mapping) {
		return true, reconcileError(
			ErrConfigurationConflict,
			"pending federation leave belongs to another mapping",
		)
	}
	if pending.Credential.PendingEnrollmentID == 0 {
		endRecovery, beginErr := daemon.BeginFederationReplicaLeaveRecovery(
			ctx,
			store,
			managed,
			mapping.SpokeProject,
			pending,
		)
		if beginErr != nil {
			switch {
			case errors.Is(beginErr, daemon.ErrFederationReplicaLeavePending):
				return true, nil
			case errors.Is(beginErr, daemon.ErrFederationReplicaCredentialIO):
				return true, reconcileError(
					ErrCredentialIO,
					"read pending enrollment recovery credential",
				)
			default:
				return true, reconcileError(
					ErrConfigurationConflict,
					"pending federation leave changed before recovery",
				)
			}
		}
		enrollment, err := recoverPendingLeaveEnrollment(
			ctx, store, hub, mapping, pending.Credential,
		)
		enrollmentID := int64(0)
		if err == nil {
			enrollmentID = enrollment.ID
		}
		_, finishErr := endRecovery(ctx, enrollmentID)
		if finishErr != nil {
			return true, reconcileError(
				ErrCredentialIO,
				"finish pending enrollment recovery",
			)
		}
		if err != nil {
			return true, err
		}
		pending, found, err = managed.FindManagedFederationCredential(
			ctx, mapping.SpokeProject,
		)
		if err != nil {
			return true, reconcileError(
				ErrCredentialIO,
				"read recovered pending enrollment",
			)
		}
		if !found || !pending.Credential.LeavePending ||
			pending.Credential.PendingEnrollmentID != enrollment.ID {
			return true, reconcileError(
				ErrConfigurationConflict,
				"pending federation leave changed during recovery",
			)
		}
	}
	if err := hub.RevokeEnrollment(ctx, pending.Credential.PendingEnrollmentID); err != nil {
		return true, safeHubError(err)
	}
	return true, nil
}

func recoverPendingLeaveEnrollment(
	ctx context.Context,
	store db.Storage,
	hub Hub,
	mapping config.FederationProjectConfig,
	credential config.FederationCredential,
) (Enrollment, error) {
	if credential.HubProjectID <= 0 || strings.TrimSpace(credential.Token) == "" {
		return Enrollment{}, reconcileError(
			ErrConfigurationConflict,
			"pending federation leave has invalid enrollment metadata",
		)
	}
	capabilities, err := federation.NormalizeCapabilities(credential.Capabilities)
	if err != nil {
		return Enrollment{}, reconcileError(
			ErrConfigurationConflict,
			"pending federation leave has invalid capabilities",
		)
	}
	project, err := store.ProjectByName(ctx, mapping.SpokeProject)
	if err != nil {
		return Enrollment{}, reconcileError(
			ErrLocalStorage,
			"read local project during pending federation leave",
		)
	}
	_, bindingErr := store.FederationBindingByProject(ctx, project.ID)
	if bindingErr != nil && !errors.Is(bindingErr, db.ErrNotFound) {
		return Enrollment{}, reconcileError(
			ErrLocalStorage,
			"read local binding during pending federation leave",
		)
	}
	request := EnrollmentRequest{
		ProjectID:                    credential.HubProjectID,
		SpokeInstanceUID:             store.InstanceUID(),
		Token:                        credential.Token,
		Capabilities:                 capabilities.API,
		Actor:                        mapping.Actor,
		AllowAdoptionSnapshotAuthors: true,
	}
	var enrollment Enrollment
	if bindingErr == nil {
		enrollment, err = hub.RotateEnrollment(ctx, request)
	} else {
		enrollment, err = hub.EnsureEnrollment(ctx, request)
	}
	if err != nil {
		return Enrollment{}, safeHubError(err)
	}
	if enrollment.ID <= 0 {
		return Enrollment{}, reconcileError(
			ErrHubValidation,
			"federation hub returned invalid enrollment metadata",
		)
	}
	return enrollment, nil
}

type mappingPreflight struct {
	localProject       db.Project
	binding            db.FederationBinding
	hasBinding         bool
	hubProject         HubProject
	credential         credentialLookup
	managedReservation config.FederationManagedCredentialReservation
	hasReservation     bool
	hubOrigin          string
	capabilities       federation.Capabilities
}

func preflightMapping(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	managed config.FederationManagedCredentialStore,
	hub Hub,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) (mappingPreflight, error) {
	var preflight mappingPreflight
	if store == nil || credentials == nil || hub == nil ||
		mapping.Hub != catalog.Name ||
		strings.TrimSpace(mapping.SpokeProject) == "" ||
		strings.TrimSpace(mapping.HubProject) == "" ||
		strings.TrimSpace(mapping.Actor) == "" {
		return preflight,
			reconcileError(ErrConfigurationConflict, "invalid federation mapping dependencies")
	}
	hubOrigin, err := config.CanonicalHTTPOrigin(catalog.URL)
	if err != nil {
		return preflight,
			reconcileError(ErrConfigurationConflict, "invalid federation hub origin")
	}
	capabilities, err := federation.NormalizeCapabilities(configCapabilities)
	if err != nil {
		return preflight,
			reconcileError(ErrConfigurationConflict, "invalid federation capability configuration")
	}
	preflight.hubOrigin = hubOrigin
	preflight.capabilities = capabilities

	localProject, err := resolveOrCreateLocalProject(ctx, store, mapping.SpokeProject)
	if err != nil {
		return preflight, err
	}
	preflight.localProject = localProject
	binding, hasBinding, err := readCompatibleBindingOrigin(
		ctx, store, localProject.ID, hubOrigin,
	)
	if err != nil {
		return preflight, err
	}
	preflight.binding = binding
	preflight.hasBinding = hasBinding
	_, hasLocalCredential, err := credentials.FederationCredential(ctx, localProject.UID)
	if err != nil {
		return preflight, reconcileError(ErrCredentialIO, "read federation credential")
	}
	managedReservation, hasManagedReservation, err := readManagedReservation(
		ctx, managed, catalog, mapping,
	)
	if err != nil {
		return preflight, err
	}
	preflight.managedReservation = managedReservation
	preflight.hasReservation = hasManagedReservation

	resolvedHubProject, resolveErr := hub.ResolveProject(ctx, mapping.HubProject)
	hubProjectExists := resolveErr == nil
	var credentialState credentialLookup
	credentialStateRead := false
	if resolveErr != nil && !hubProjectNotFound(resolveErr) {
		return preflight, safeHubError(resolveErr)
	}
	if !hubProjectExists {
		if hasBinding {
			return preflight,
				reconcileError(ErrBindingConflict, "bound federation project is missing from hub")
		}
		if hasLocalCredential || hasManagedReservation {
			return preflight,
				reconcileError(ErrConfigurationConflict, "credentialed federation project is missing from hub")
		}
	} else {
		if err := validateResolvedHubProject(resolvedHubProject); err != nil {
			return preflight, err
		}
		if hasBinding &&
			(binding.HubProjectUID != resolvedHubProject.UID ||
				binding.HubProjectID != resolvedHubProject.ID) {
			return preflight,
				reconcileError(ErrBindingConflict, "existing federation binding targets another project")
		}
		if hasManagedReservation {
			if !credentialMatchesTarget(
				managedReservation.Credential, hubOrigin, resolvedHubProject.ID,
				catalog.AllowInsecure, capabilities.API,
			) {
				return preflight, reconcileError(
					ErrConfigurationConflict,
					"managed federation reservation targets a different hub",
				)
			}
			if managedReservation.ProjectUID != resolvedHubProject.UID {
				return preflight, reconcileError(
					ErrConfigurationConflict,
					"managed federation reservation uses another hub project UID",
				)
			}
		}
		credentialState, err = readCredentialState(
			ctx, credentials, localProject.UID, resolvedHubProject.UID,
		)
		if err != nil {
			return preflight, err
		}
		credentialStateRead = true
		if hasManagedReservation &&
			(!credentialState.found ||
				credentialState.key != managedReservation.ProjectUID ||
				credentialState.credential != managedReservation.Credential) {
			return preflight, reconcileError(
				ErrConfigurationConflict,
				"managed federation reservation differs from credential state",
			)
		}
		if credentialState.found &&
			!credentialMatchesTarget(
				credentialState.credential, hubOrigin, resolvedHubProject.ID,
				catalog.AllowInsecure, capabilities.API,
			) {
			return preflight, reconcileError(
				ErrConfigurationConflict,
				"existing federation credential targets a different hub",
			)
		}
		if err := validateHubProjectOwnership(ctx, store, localProject.ID, resolvedHubProject.UID); err != nil {
			return preflight, err
		}
	}

	hubProject, err := hub.EnsureProject(ctx, mapping.HubProject, mapping.Actor)
	if err != nil {
		return preflight, safeHubError(err)
	}
	if hubProject.ID <= 0 ||
		!katauid.Valid(hubProject.UID) ||
		hubProject.ReplayHorizonEventID <= 0 {
		return preflight,
			reconcileError(ErrHubValidation, "federation hub returned invalid project metadata")
	}
	if hubProjectExists &&
		(hubProject.ID != resolvedHubProject.ID ||
			hubProject.UID != resolvedHubProject.UID ||
			hubProject.Name != resolvedHubProject.Name) {
		return preflight,
			reconcileError(ErrHubValidation, "federation hub project changed during enable")
	}
	if !hubProjectExists {
		if err := validateHubProjectOwnership(ctx, store, localProject.ID, hubProject.UID); err != nil {
			return preflight, err
		}
	}
	preflight.hubProject = hubProject

	if !credentialStateRead {
		credentialState, err = readCredentialState(
			ctx, credentials, localProject.UID, hubProject.UID,
		)
		if err != nil {
			return preflight, err
		}
	}
	if credentialState.found &&
		!credentialMatchesTarget(
			credentialState.credential, hubOrigin, hubProject.ID,
			catalog.AllowInsecure, capabilities.API,
		) {
		return preflight, reconcileError(
			ErrConfigurationConflict,
			"existing federation credential targets a different hub",
		)
	}

	if credentialState.found && credentialState.credential.ManagedByConfig {
		if !hasManagedReservation {
			return preflight, reconcileError(
				ErrConfigurationConflict,
				"managed federation credential is not reserved for this mapping",
			)
		}
		if !credentialManagementMatches(credentialState.credential, catalog, mapping) {
			return preflight, reconcileError(
				ErrConfigurationConflict,
				"existing managed federation credential belongs to another mapping",
			)
		}
	}

	if !credentialState.found {
		token, tokenErr := db.NewFederationToken()
		if tokenErr != nil {
			return preflight,
				reconcileError(ErrCredentialIO, "generate federation enrollment credential")
		}
		pendingCredential := config.FederationCredential{
			HubURL:       hubOrigin,
			HubProjectID: hubProject.ID,
			Token:        token,
			Capabilities: capabilities.API,
			// Empty marks a durable token whose authoritative actor has not
			// yet been returned by the hub. This distinguishes an exact
			// lost-response replay from a conflicting established identity.
			Actor:            "",
			AllowInsecure:    catalog.AllowInsecure,
			ManagedByConfig:  true,
			HubCatalog:       catalog.Name,
			HubProjectName:   mapping.HubProject,
			RequestedActor:   mapping.Actor,
			SpokeProjectName: mapping.SpokeProject,
		}
		if err := reserveManagedFederationCredential(
			ctx, store, managed, mapping.SpokeProject, hubProject.UID,
			pendingCredential, preflight.hasBinding,
		); err != nil {
			return preflight, err
		}
		preflight.managedReservation = config.FederationManagedCredentialReservation{
			ProjectUID: hubProject.UID,
			Credential: pendingCredential,
		}
		preflight.hasReservation = true
		credentialState = credentialLookup{
			credential: pendingCredential,
			key:        hubProject.UID,
			found:      true,
		}
	}
	preflight.credential = credentialState
	return preflight, nil
}

type mappingEnrollment struct {
	credential config.FederationCredential
	id         int64
}

func ensureMappingEnrollment(
	ctx context.Context,
	store db.Storage,
	hub Hub,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
	preflight mappingPreflight,
) (mappingEnrollment, error) {
	credential := preflight.credential.credential
	if preflight.hasBinding {
		pendingReplacement := credential.ManagedByConfig &&
			strings.TrimSpace(credential.Actor) == ""
		if !pendingReplacement {
			if strings.TrimSpace(preflight.binding.Actor) != "" &&
				strings.TrimSpace(preflight.binding.Actor) != strings.TrimSpace(credential.Actor) {
				return mappingEnrollment{}, reconcileError(
					ErrBindingConflict,
					"existing federation binding actor differs from credential",
				)
			}
			return mappingEnrollment{
				credential: withManagementMetadata(credential, catalog, mapping),
			}, nil
		}
	}

	enrollmentRequest := EnrollmentRequest{
		ProjectID:                    preflight.hubProject.ID,
		SpokeInstanceUID:             store.InstanceUID(),
		Token:                        credential.Token,
		Capabilities:                 preflight.capabilities.API,
		Actor:                        mapping.Actor,
		AllowAdoptionSnapshotAuthors: true,
	}
	var enrollment Enrollment
	var err error
	if preflight.hasBinding {
		enrollment, err = hub.RotateEnrollment(ctx, enrollmentRequest)
	} else {
		enrollment, err = hub.EnsureEnrollment(ctx, enrollmentRequest)
	}
	if err != nil {
		return mappingEnrollment{}, safeHubError(err)
	}
	enrollmentActor := strings.TrimSpace(enrollment.Actor)
	if enrollment.ID <= 0 || db.ValidateTokenActor(enrollmentActor) != nil {
		return mappingEnrollment{},
			reconcileError(ErrHubValidation, "federation hub returned invalid enrollment metadata")
	}

	credential.HubURL = preflight.hubOrigin
	credential.HubProjectID = preflight.hubProject.ID
	credential.Capabilities = preflight.capabilities.API
	credential.Actor = enrollmentActor
	credential.AllowInsecure = catalog.AllowInsecure
	credential = withManagementMetadata(credential, catalog, mapping)
	return mappingEnrollment{credential: credential, id: enrollment.ID}, nil
}

func convergeLocalMapping(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	wake func(),
	mapping config.FederationProjectConfig,
	preflight mappingPreflight,
	credential config.FederationCredential,
) (bool, error) {
	if preflight.hasBinding &&
		preflight.credential.credential.ManagedByConfig &&
		bindingOperational(preflight.binding, preflight.credential.credential) {
		return false, nil
	}

	params := daemon.EnsureFederationReplicaParams{
		HubURL:               preflight.hubOrigin,
		HubProjectID:         preflight.hubProject.ID,
		HubProjectUID:        preflight.hubProject.UID,
		ProjectName:          mapping.SpokeProject,
		ReplayHorizonEventID: preflight.hubProject.ReplayHorizonEventID,
		Credential:           credential,
		PushEnabled:          true,
		AdoptExisting:        true,
	}
	if preflight.credential.key != "" &&
		preflight.credential.key != preflight.hubProject.UID {
		params.CredentialRekey = &daemon.FederationReplicaCredentialRekeySource{
			ProjectUID: preflight.credential.key,
			Expected:   preflight.credential.credential,
		}
	} else if preflight.hasReservation {
		params.ManagedReservation = &daemon.FederationReplicaManagedReservation{
			ProjectUID: preflight.managedReservation.ProjectUID,
			Expected:   preflight.managedReservation.Credential,
		}
	}

	localWake := wake
	if preflight.hasBinding &&
		bindingOperational(preflight.binding, preflight.credential.credential) {
		localWake = nil
	}
	_, err := daemon.EnsureFederationReplica(ctx, store, credentials, localWake, params)
	if err != nil {
		switch {
		case errors.Is(err, daemon.ErrFederationReplicaReservationChanged):
			return true, reconcileError(
				ErrConfigurationConflict,
				"federation credential changed during local convergence",
			)
		case errors.Is(err, daemon.ErrFederationReplicaCredentialIO):
			return false, reconcileError(ErrCredentialIO, "update local federation credential")
		case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict),
			errors.Is(err, config.ErrFederationCredentialConflict):
			return false, reconcileError(
				ErrConfigurationConflict,
				"federation credential changed during local convergence",
			)
		case errors.Is(err, daemon.ErrFederationReplicaBindingConflict):
			return false, reconcileError(ErrBindingConflict, "existing federation binding is incompatible")
		default:
			return false, reconcileError(ErrLocalStorage, "ensure local federation replica")
		}
	}

	return false, nil
}

func readManagedReservation(
	ctx context.Context,
	managed config.FederationManagedCredentialStore,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) (config.FederationManagedCredentialReservation, bool, error) {
	match, found, err := managed.FindManagedFederationCredential(ctx, mapping.SpokeProject)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return config.FederationManagedCredentialReservation{}, false,
				reconcileError(ErrConfigurationConflict, "managed federation reservations disagree")
		}
		return config.FederationManagedCredentialReservation{}, false,
			reconcileError(ErrCredentialIO, "read managed federation reservation")
	}
	if !found {
		return config.FederationManagedCredentialReservation{}, false, nil
	}
	if strings.TrimSpace(match.ProjectUID) == "" ||
		!match.Credential.ManagedByConfig ||
		match.Credential.SpokeProjectName != mapping.SpokeProject ||
		!credentialManagementMatches(match.Credential, catalog, mapping) {
		return config.FederationManagedCredentialReservation{}, false,
			reconcileError(ErrConfigurationConflict, "managed federation reservation metadata differs")
	}
	return match, true, nil
}

func reserveManagedFederationCredential(
	ctx context.Context,
	store db.Storage,
	managed config.FederationManagedCredentialStore,
	projectName, hubProjectUID string,
	credential config.FederationCredential,
	expectedBound bool,
) error {
	err := daemon.ReserveFederationReplicaCredential(
		ctx,
		store,
		managed,
		daemon.ReserveFederationReplicaCredentialParams{
			HubProjectUID: hubProjectUID,
			ProjectName:   projectName,
			Credential:    credential,
			ExpectedBound: expectedBound,
		},
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, daemon.ErrFederationReplicaCredentialIO):
		return reconcileError(ErrCredentialIO, "reserve federation enrollment credential")
	case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict):
		return reconcileError(ErrConfigurationConflict, "federation credential reservation conflict")
	case errors.Is(err, daemon.ErrFederationReplicaBindingConflict):
		return reconcileError(ErrBindingConflict, "local federation binding changed before credential reservation")
	default:
		return reconcileError(ErrLocalStorage, "reserve federation enrollment credential")
	}
}

func resolveOrCreateLocalProject(
	ctx context.Context, store db.Storage, name string,
) (db.Project, error) {
	project, err := store.ProjectByName(ctx, name)
	if err == nil {
		return project, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return db.Project{}, reconcileError(ErrLocalStorage, "resolve local federation project")
	}
	project, err = store.CreateProject(ctx, name)
	if err == nil {
		return project, nil
	}
	// A concurrent local creator can win the name after the first read.
	project, retryErr := store.ProjectByName(ctx, name)
	if retryErr == nil {
		return project, nil
	}
	return db.Project{}, reconcileError(ErrLocalStorage, "create local federation project")
}

func readCompatibleBindingOrigin(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	hubOrigin string,
) (db.FederationBinding, bool, error) {
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return db.FederationBinding{}, false, nil
	}
	if err != nil {
		return db.FederationBinding{}, false,
			reconcileError(ErrLocalStorage, "read local federation binding")
	}
	if binding.Role != db.FederationRoleSpoke {
		return db.FederationBinding{}, false,
			reconcileError(ErrBindingConflict, "existing federation binding has another role")
	}
	existingOrigin, err := config.CanonicalHTTPOrigin(binding.HubURL)
	if err != nil || existingOrigin != hubOrigin {
		return db.FederationBinding{}, false,
			reconcileError(ErrBindingConflict, "existing federation binding targets another origin")
	}
	return binding, true, nil
}

func hubProjectNotFound(err error) bool {
	var hubErr *HubError
	return errors.As(err, &hubErr) && hubErr != nil && hubErr.StatusCode == http.StatusNotFound
}

func validateResolvedHubProject(project HubProject) error {
	if project.ID <= 0 || !katauid.Valid(project.UID) || strings.TrimSpace(project.Name) == "" {
		return reconcileError(ErrHubValidation, "federation hub returned invalid project metadata")
	}
	return nil
}

func validateHubProjectOwnership(
	ctx context.Context,
	store db.Storage,
	localProjectID int64,
	hubProjectUID string,
) error {
	project, err := store.ProjectByUID(ctx, hubProjectUID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return reconcileError(ErrLocalStorage, "check local federation project UID ownership")
	}
	if project.ID != localProjectID {
		return reconcileError(ErrBindingConflict, "hub project UID belongs to another local project")
	}
	return nil
}

type credentialLookup struct {
	credential config.FederationCredential
	key        string
	found      bool
}

func readCredentialState(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	localProjectUID, hubProjectUID string,
) (credentialLookup, error) {
	finalCredential, finalFound, err := credentials.FederationCredential(ctx, hubProjectUID)
	if err != nil {
		return credentialLookup{}, reconcileError(ErrCredentialIO, "read federation credential")
	}
	if localProjectUID == hubProjectUID {
		return credentialLookup{
			credential: finalCredential,
			key:        hubProjectUID,
			found:      finalFound,
		}, nil
	}
	localCredential, localFound, err := credentials.FederationCredential(ctx, localProjectUID)
	if err != nil {
		return credentialLookup{}, reconcileError(ErrCredentialIO, "read federation credential")
	}
	if finalFound && localFound && finalCredential != localCredential {
		return credentialLookup{}, reconcileError(
			ErrConfigurationConflict,
			"multiple federation credentials disagree for one local project",
		)
	}
	if localFound {
		return credentialLookup{
			credential: localCredential,
			key:        localProjectUID,
			found:      true,
		}, nil
	}
	return credentialLookup{
		credential: finalCredential,
		key:        hubProjectUID,
		found:      finalFound,
	}, nil
}

func credentialMatchesTarget(
	credential config.FederationCredential,
	hubOrigin string,
	hubProjectID int64,
	allowInsecure bool,
	apiCapabilities string,
) bool {
	credentialOrigin, err := config.CanonicalHTTPOrigin(credential.HubURL)
	if err != nil || credentialOrigin != hubOrigin ||
		credential.HubProjectID != hubProjectID ||
		credential.Token == "" ||
		credential.AllowInsecure != allowInsecure {
		return false
	}
	capabilities, err := db.CanonicalFederationCapabilities(credential.Capabilities)
	return err == nil && capabilities == apiCapabilities
}

func bindingOperational(
	binding db.FederationBinding,
	credential config.FederationCredential,
) bool {
	bindingActor := strings.TrimSpace(binding.Actor)
	return binding.Enabled &&
		binding.PushEnabled &&
		bindingActor != "" &&
		bindingActor == strings.TrimSpace(credential.Actor)
}

func credentialManagementMatches(
	credential config.FederationCredential,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) bool {
	return credential.HubCatalog == catalog.Name &&
		credential.HubProjectName == mapping.HubProject &&
		credential.RequestedActor == mapping.Actor &&
		(credential.SpokeProjectName == "" ||
			credential.SpokeProjectName == mapping.SpokeProject)
}

func withManagementMetadata(
	credential config.FederationCredential,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) config.FederationCredential {
	credential.ManagedByConfig = true
	credential.HubCatalog = catalog.Name
	credential.HubProjectName = mapping.HubProject
	credential.RequestedActor = mapping.Actor
	credential.SpokeProjectName = mapping.SpokeProject
	return credential
}

func safeHubError(err error) error {
	switch {
	case errors.Is(err, ErrHubAuthentication),
		errors.Is(err, ErrHubValidation),
		errors.Is(err, ErrHubUnavailable):
		return err
	default:
		return reconcileError(ErrHubUnavailable, "federation hub request failed")
	}
}

func reconcileError(kind error, message string) *reconciliationError {
	return &reconciliationError{kind: kind, message: message}
}
