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
			"federation config reconciliation state=%s category=%s status=%d",
			stateName, category, status,
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
	case errors.Is(err, ErrHubUnavailable):
		return "hub_unavailable", status
	case errors.Is(err, ErrHubAuthentication):
		return "hub_authentication", status
	case errors.Is(err, ErrHubValidation):
		return "hub_validation", status
	default:
		return "local_storage", status
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
	if store == nil || credentials == nil || hub == nil ||
		mapping.Hub != catalog.Name ||
		strings.TrimSpace(mapping.SpokeProject) == "" ||
		strings.TrimSpace(mapping.HubProject) == "" ||
		strings.TrimSpace(mapping.Actor) == "" {
		return reconcileError(ErrConfigurationConflict, "invalid federation mapping dependencies")
	}
	hubOrigin, err := config.CanonicalHTTPOrigin(catalog.URL)
	if err != nil {
		return reconcileError(ErrConfigurationConflict, "invalid federation hub origin")
	}
	capabilities, err := federation.NormalizeCapabilities(configCapabilities)
	if err != nil {
		return reconcileError(ErrConfigurationConflict, "invalid federation capability configuration")
	}

	localProject, err := resolveOrCreateLocalProject(ctx, store, mapping.SpokeProject)
	if err != nil {
		return err
	}
	binding, hasBinding, err := readCompatibleBindingOrigin(
		ctx, store, localProject.ID, hubOrigin,
	)
	if err != nil {
		return err
	}
	_, hasLocalCredential, err := credentials.FederationCredential(ctx, localProject.UID)
	if err != nil {
		return reconcileError(ErrCredentialIO, "read federation credential")
	}
	reservationMatch, hasManagedReservation, err := readManagedReservation(
		ctx, credentials, catalog, mapping,
	)
	if err != nil {
		return err
	}

	resolvedHubProject, resolveErr := hub.ResolveProject(ctx, mapping.HubProject)
	hubProjectExists := resolveErr == nil
	var credentialState credentialLookup
	credentialStateRead := false
	if resolveErr != nil && !hubProjectNotFound(resolveErr) {
		return safeHubError(resolveErr)
	}
	if !hubProjectExists {
		if hasBinding {
			return reconcileError(ErrBindingConflict, "bound federation project is missing from hub")
		}
		if hasLocalCredential || hasManagedReservation {
			return reconcileError(ErrConfigurationConflict, "credentialed federation project is missing from hub")
		}
	} else {
		if err := validateResolvedHubProject(resolvedHubProject); err != nil {
			return err
		}
		if hasBinding &&
			(binding.HubProjectUID != resolvedHubProject.UID ||
				binding.HubProjectID != resolvedHubProject.ID) {
			return reconcileError(ErrBindingConflict, "existing federation binding targets another project")
		}
		if hasManagedReservation {
			if !credentialMatchesTarget(
				reservationMatch.Credential, hubOrigin, resolvedHubProject.ID,
				catalog.AllowInsecure, capabilities.API,
			) {
				return reconcileError(
					ErrConfigurationConflict,
					"managed federation reservation targets a different hub",
				)
			}
			for _, projectUID := range reservationMatch.ProjectUIDs {
				if projectUID != localProject.UID &&
					projectUID != resolvedHubProject.UID {
					return reconcileError(
						ErrConfigurationConflict,
						"managed federation reservation uses another hub project UID",
					)
				}
			}
		}
		credentialState, err = readCredentialState(
			ctx, credentials, localProject.UID, resolvedHubProject.UID,
		)
		if err != nil {
			return err
		}
		credentialStateRead = true
		if hasManagedReservation &&
			(!credentialState.found ||
				credentialState.credential != reservationMatch.Credential) {
			return reconcileError(
				ErrConfigurationConflict,
				"managed federation reservation differs from credential aliases",
			)
		}
		if credentialState.found &&
			!credentialMatchesTarget(
				credentialState.credential, hubOrigin, resolvedHubProject.ID,
				catalog.AllowInsecure, capabilities.API,
			) {
			return reconcileError(
				ErrConfigurationConflict,
				"existing federation credential targets a different hub",
			)
		}
		if err := validateHubProjectOwnership(ctx, store, localProject.ID, resolvedHubProject.UID); err != nil {
			return err
		}
	}

	hubProject, err := hub.EnsureProject(ctx, mapping.HubProject, mapping.Actor)
	if err != nil {
		return safeHubError(err)
	}
	if hubProject.ID <= 0 ||
		!katauid.Valid(hubProject.UID) ||
		hubProject.ReplayHorizonEventID <= 0 {
		return reconcileError(ErrHubValidation, "federation hub returned invalid project metadata")
	}
	if hubProjectExists &&
		(hubProject.ID != resolvedHubProject.ID ||
			hubProject.UID != resolvedHubProject.UID ||
			hubProject.Name != resolvedHubProject.Name) {
		return reconcileError(ErrHubValidation, "federation hub project changed during enable")
	}
	if !hubProjectExists {
		if err := validateHubProjectOwnership(ctx, store, localProject.ID, hubProject.UID); err != nil {
			return err
		}
	}

	if !credentialStateRead {
		credentialState, err = readCredentialState(
			ctx, credentials, localProject.UID, hubProject.UID,
		)
		if err != nil {
			return err
		}
	}
	if credentialState.found &&
		!credentialMatchesTarget(
			credentialState.credential, hubOrigin, hubProject.ID,
			catalog.AllowInsecure, capabilities.API,
		) {
		return reconcileError(
			ErrConfigurationConflict,
			"existing federation credential targets a different hub",
		)
	}

	if hasBinding && credentialState.found {
		credential := credentialState.credential
		if credential.ManagedByConfig &&
			!credentialManagementMatches(credential, catalog, mapping) {
			return reconcileError(
				ErrConfigurationConflict,
				"existing managed federation credential belongs to another mapping",
			)
		}
		pendingReplacement := credential.ManagedByConfig &&
			strings.TrimSpace(credential.Actor) == ""
		if !pendingReplacement {
			if strings.TrimSpace(binding.Actor) != "" &&
				strings.TrimSpace(binding.Actor) != strings.TrimSpace(credential.Actor) {
				return reconcileError(ErrBindingConflict, "existing federation binding actor differs from credential")
			}
			operational := bindingOperational(binding, credential)
			if credential.ManagedByConfig {
				if operational {
					return nil
				}
			} else {
				adopted := withManagementMetadata(credential, catalog, mapping)
				repairWake := wake
				if operational {
					repairWake = nil
				}
				return ensureLocalManualReplica(
					ctx,
					store,
					credentials,
					repairWake,
					localProject,
					hubProject,
					hubOrigin,
					credentialState,
					adopted,
				)
			}
		}
	}

	if !hasBinding &&
		credentialState.found &&
		credentialState.key == hubProject.UID &&
		localProject.UID != hubProject.UID {
		if err := reserveFederationCredentialAliases(
			ctx, store, credentials, localProject.Name, hubProject.UID,
			credentialState.credential,
		); err != nil {
			return err
		}
		credentialState.key = localProject.UID
	}

	credential := credentialState.credential
	if !credentialState.found {
		token, tokenErr := db.NewFederationToken()
		if tokenErr != nil {
			return reconcileError(ErrCredentialIO, "generate federation enrollment credential")
		}
		credential = config.FederationCredential{
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
		if err := reserveFederationCredentialAliases(
			ctx, store, credentials, mapping.SpokeProject, hubProject.UID,
			credential,
		); err != nil {
			return err
		}
		credentialState = credentialLookup{
			credential: credential,
			key:        localProject.UID,
			found:      true,
		}
	}

	enrollmentRequest := EnrollmentRequest{
		ProjectID:                    hubProject.ID,
		SpokeInstanceUID:             store.InstanceUID(),
		Token:                        credential.Token,
		Capabilities:                 capabilities.API,
		Actor:                        mapping.Actor,
		AllowAdoptionSnapshotAuthors: true,
	}
	var enrollment Enrollment
	if hasBinding {
		enrollment, err = hub.RotateEnrollment(ctx, enrollmentRequest)
	} else {
		enrollment, err = hub.EnsureEnrollment(ctx, enrollmentRequest)
	}
	if err != nil {
		return safeHubError(err)
	}
	enrollmentActor := strings.TrimSpace(enrollment.Actor)
	if enrollment.ID <= 0 || db.ValidateTokenActor(enrollmentActor) != nil {
		return reconcileError(ErrHubValidation, "federation hub returned invalid enrollment metadata")
	}

	credential.HubURL = hubOrigin
	credential.HubProjectID = hubProject.ID
	credential.Capabilities = capabilities.API
	credential.Actor = enrollmentActor
	credential.AllowInsecure = catalog.AllowInsecure
	credential = withManagementMetadata(credential, catalog, mapping)

	var credentialRekey *daemon.FederationReplicaCredentialRekeySource
	if credentialState.key != "" && credentialState.key != hubProject.UID {
		credentialRekey = &daemon.FederationReplicaCredentialRekeySource{
			ProjectUID: credentialState.key,
			Expected:   credentialState.credential,
		}
	}

	_, err = daemon.EnsureFederationReplica(
		ctx, store, classifiedCredentialStore{delegate: credentials}, wake,
		daemon.EnsureFederationReplicaParams{
			HubURL:                 hubOrigin,
			HubProjectID:           hubProject.ID,
			HubProjectUID:          hubProject.UID,
			ProjectName:            mapping.SpokeProject,
			ReplayHorizonEventID:   hubProject.ReplayHorizonEventID,
			BaselineThroughEventID: hubProject.BaselineThroughEventID,
			Credential:             credential,
			CredentialRekey:        credentialRekey,
			PushEnabled:            true,
			AdoptExisting:          true,
		},
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrCredentialIO):
			return reconcileError(ErrCredentialIO, "store local federation credential")
		case errors.Is(err, config.ErrFederationCredentialConflict):
			return reconcileError(ErrConfigurationConflict, "federation credential changed during atomic rekey")
		case errors.Is(err, daemon.ErrFederationReplicaBindingConflict):
			return reconcileError(ErrBindingConflict, "existing federation binding is incompatible")
		case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict):
			return reconcileError(ErrConfigurationConflict, "existing federation credential is incompatible")
		default:
			return reconcileError(ErrLocalStorage, "ensure local federation replica")
		}
	}

	return nil
}

func readManagedReservation(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	catalog config.CatalogDaemonConfig,
	mapping config.FederationProjectConfig,
) (config.FederationCredentialReservationMatch, bool, error) {
	finder, ok := credentials.(config.FederationCredentialReservationFinder)
	if !ok {
		return config.FederationCredentialReservationMatch{}, false, nil
	}
	match, found, err := finder.FederationCredentialReservationForProject(
		ctx, mapping.SpokeProject,
	)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return config.FederationCredentialReservationMatch{}, false,
				reconcileError(ErrConfigurationConflict, "managed federation reservations disagree")
		}
		return config.FederationCredentialReservationMatch{}, false,
			reconcileError(ErrCredentialIO, "read managed federation reservation")
	}
	if !found {
		return config.FederationCredentialReservationMatch{}, false, nil
	}
	if len(match.ProjectUIDs) == 0 ||
		!match.Credential.ManagedByConfig ||
		match.Credential.SpokeProjectName != mapping.SpokeProject ||
		!credentialManagementMatches(match.Credential, catalog, mapping) {
		return config.FederationCredentialReservationMatch{}, false,
			reconcileError(ErrConfigurationConflict, "managed federation reservation metadata differs")
	}
	return match, true, nil
}

func reserveFederationCredentialAliases(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectName, hubProjectUID string,
	credential config.FederationCredential,
) error {
	err := daemon.ReserveFederationReplicaCredential(
		ctx,
		store,
		classifiedCredentialStore{delegate: credentials},
		daemon.ReserveFederationReplicaCredentialParams{
			HubProjectUID: hubProjectUID,
			ProjectName:   projectName,
			Credential:    credential,
		},
	)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCredentialIO):
		return reconcileError(ErrCredentialIO, "reserve federation enrollment credential")
	case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict):
		return reconcileError(ErrConfigurationConflict, "federation credential reservation conflict")
	default:
		return reconcileError(ErrLocalStorage, "reserve federation enrollment credential")
	}
}

func ensureLocalManualReplica(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	wake func(),
	localProject db.Project,
	hubProject HubProject,
	hubOrigin string,
	credentialState credentialLookup,
	credential config.FederationCredential,
) error {
	var credentialRekey *daemon.FederationReplicaCredentialRekeySource
	if credentialState.key != "" && credentialState.key != hubProject.UID {
		credentialRekey = &daemon.FederationReplicaCredentialRekeySource{
			ProjectUID: credentialState.key,
			Expected:   credentialState.credential,
		}
	}
	_, err := daemon.EnsureFederationReplica(
		ctx, store, classifiedCredentialStore{delegate: credentials}, wake,
		daemon.EnsureFederationReplicaParams{
			HubURL:                 hubOrigin,
			HubProjectID:           hubProject.ID,
			HubProjectUID:          hubProject.UID,
			ProjectName:            localProject.Name,
			ReplayHorizonEventID:   hubProject.ReplayHorizonEventID,
			BaselineThroughEventID: hubProject.BaselineThroughEventID,
			Credential:             credential,
			CredentialRekey:        credentialRekey,
			PushEnabled:            true,
			AdoptExisting:          true,
		},
	)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrCredentialIO):
		return reconcileError(ErrCredentialIO, "store local federation credential")
	case errors.Is(err, config.ErrFederationCredentialConflict):
		return reconcileError(ErrConfigurationConflict, "federation credential changed during atomic rekey")
	case errors.Is(err, daemon.ErrFederationReplicaBindingConflict):
		return reconcileError(ErrBindingConflict, "existing federation binding is incompatible")
	case errors.Is(err, daemon.ErrFederationReplicaCredentialConflict):
		return reconcileError(ErrConfigurationConflict, "existing federation credential is incompatible")
	default:
		return reconcileError(ErrLocalStorage, "ensure local federation replica")
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

type classifiedCredentialStore struct {
	delegate config.FederationCredentialStore
}

func (s classifiedCredentialStore) FederationCredential(
	ctx context.Context, projectUID string,
) (config.FederationCredential, bool, error) {
	credential, ok, err := s.delegate.FederationCredential(ctx, projectUID)
	if err != nil {
		return config.FederationCredential{}, false,
			reconcileError(ErrCredentialIO, "read federation credential")
	}
	return credential, ok, nil
}

func (s classifiedCredentialStore) StoreFederationCredential(
	ctx context.Context, projectUID string, credential config.FederationCredential,
) error {
	if err := s.delegate.StoreFederationCredential(ctx, projectUID, credential); err != nil {
		return reconcileError(ErrCredentialIO, "store federation credential")
	}
	return nil
}

func (s classifiedCredentialStore) DeleteFederationCredential(
	ctx context.Context, projectUID string,
) error {
	if err := s.delegate.DeleteFederationCredential(ctx, projectUID); err != nil {
		return reconcileError(ErrCredentialIO, "delete federation credential")
	}
	return nil
}

func (s classifiedCredentialStore) RekeyFederationCredential(
	ctx context.Context, rekey config.FederationCredentialRekey,
) error {
	rekeyer, ok := s.delegate.(config.FederationCredentialRekeyer)
	if !ok {
		return reconcileError(
			ErrCredentialIO,
			"credential store does not support atomic federation rekey",
		)
	}
	if err := rekeyer.RekeyFederationCredential(ctx, rekey); err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return err
		}
		return reconcileError(ErrCredentialIO, "atomically rekey federation credential")
	}
	return nil
}

func (s classifiedCredentialStore) ReserveFederationCredentials(
	ctx context.Context,
	reservation config.FederationCredentialReservation,
) error {
	reserver, ok := s.delegate.(config.FederationCredentialReserver)
	if !ok {
		return reconcileError(
			ErrCredentialIO,
			"credential store does not support atomic federation reservation",
		)
	}
	if err := reserver.ReserveFederationCredentials(ctx, reservation); err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return err
		}
		return reconcileError(ErrCredentialIO, "atomically reserve federation credential")
	}
	return nil
}

func (s classifiedCredentialStore) FederationCredentialReservationForProject(
	ctx context.Context,
	projectName string,
) (config.FederationCredentialReservationMatch, bool, error) {
	finder, ok := s.delegate.(config.FederationCredentialReservationFinder)
	if !ok {
		return config.FederationCredentialReservationMatch{}, false, nil
	}
	match, found, err := finder.FederationCredentialReservationForProject(
		ctx, projectName,
	)
	if err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return config.FederationCredentialReservationMatch{}, false, err
		}
		return config.FederationCredentialReservationMatch{}, false,
			reconcileError(ErrCredentialIO, "read managed federation reservation")
	}
	return match, found, nil
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
