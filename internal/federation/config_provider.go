package federation

import (
	"context"
	"errors"
	"slices"
	"strings"

	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemon"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/federationprovider"
)

// status comes only from a validated provider response, never its prose.
type providerDecisionError struct{ status federationprovider.Status }

func (e *providerDecisionError) Error() string {
	return "federation credential provider: " + string(e.status)
}

func reconcileProviderMapping(
	ctx context.Context, store db.Storage, credentials config.FederationCredentialStore,
	catalog config.CatalogDaemonConfig, mapping config.FederationProjectConfig,
	wake func(), projectEventSink func(db.Event),
) (resultErr error) {
	if store == nil || credentials == nil || mapping.Hub != catalog.Name {
		return reconcileError(ErrConfigurationConflict, "invalid federation mapping dependencies")
	}
	if err := config.ValidateFederationAuthentication(mapping, catalog); err != nil {
		return reconcileError(ErrConfigurationConflict, "invalid federation provider configuration")
	}
	if leaving, err := reconcileProviderLeave(ctx, store, credentials, mapping.SpokeProject, nil); leaving || err != nil {
		return err
	}
	if daemon.FederationReplicaMappingSuppressed(store, mapping.SpokeProject) {
		return nil
	}
	_, event, err := resolveOrCreateLocalProject(ctx, store, mapping.SpokeProject)
	if event != nil && projectEventSink != nil {
		projectEventSink(*event)
	}
	if err != nil {
		return err
	}
	reservation, err := daemon.AuthorizeFederationProvider(ctx, store, credentials, catalog, mapping)
	if err != nil {
		return providerReconciliationError(err)
	}
	credential := reservation.Credential
	decision := credential.Provider
	if decision.Status != federationprovider.StatusReady {
		return &providerDecisionError{status: decision.Status}
	}
	finish, err := daemon.BeginFederationReplicaHubOperation(ctx, store, credentials, mapping.SpokeProject, reservation)
	if err != nil {
		return providerReconciliationError(err)
	}
	defer func() {
		_, finishErr := finish(ctx, 0)
		resultErr = errors.Join(resultErr, finishErr)
	}()
	client, err := NewClient(ctx, credential.HubURL, credential.Token, clientpkg.Opts{Timeout: defaultFederationClientTimeout})
	if err != nil {
		return reconcileError(ErrConfigurationConflict, "invalid federation provider endpoint")
	}
	metadata, err := client.ProjectFederation(ctx, credential.HubProjectID)
	if err != nil {
		// A hub's error body is not safe to put in reconciliation logs.
		return reconcileError(ErrHubUnavailable, "read approved federation metadata")
	}
	// The provider resolves the requested key to stable IDs. Its key may differ
	// from Kata's internal project name, which may also change after approval.
	if metadata.ProjectID != credential.HubProjectID || metadata.ProjectUID != decision.HubProjectUID || metadata.ReplayHorizonEventID <= 0 {
		return reconcileError(ErrBindingConflict, "federation metadata differs from the approved project")
	}
	params := daemon.EnsureFederationReplicaParams{
		HubURL: credential.HubURL, HubProjectID: credential.HubProjectID, HubProjectUID: decision.HubProjectUID,
		ProjectName: mapping.SpokeProject, ReplayHorizonEventID: metadata.ReplayHorizonEventID,
		Credential: credential, PushEnabled: slices.Contains(strings.Split(credential.Capabilities, ","), "push"),
		AdoptExisting: decision.Intent == federationprovider.IntentMigrate, AttachEmpty: decision.Intent != federationprovider.IntentMigrate,
		ManagedReservation: &daemon.FederationReplicaManagedReservation{ProjectUID: reservation.ProjectUID, Expected: credential},
		ProjectEventSink:   projectEventSink,
	}
	if reservation.ProjectUID != decision.HubProjectUID {
		params.CredentialRekey = &daemon.FederationReplicaCredentialRekeySource{ProjectUID: reservation.ProjectUID, Expected: credential}
	}
	_, err = daemon.EnsureFederationReplica(ctx, store, credentials, wake, params)
	return providerReconciliationError(err)
}

// Config is loaded at daemon start. Include retained provider requests whose
// mapping was removed, even when no active federation mappings remain. Ordinary
// credentials and mappings changed in place are not removal authority.
func (r *Reconciler) findRemovedProviderMappings(ctx context.Context) error {
	managed, ok := r.credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return nil
	}
	reservations, err := managed.ListManagedFederationCredentials(ctx)
	if err != nil {
		return reconcileError(ErrCredentialIO, "read retained provider cleanup")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, saved := range reservations {
		if saved.Credential.Provider == nil {
			continue
		}
		retained, err := r.providerMappingRetained(ctx, saved)
		if err == nil && retained {
			continue
		}
		// Retry an unreadable project in its cleanup attempt, not by stopping
		// the controller. Each attempt rechecks that the mapping is absent.
		r.targets = append(r.targets, Target{
			Mapping: config.FederationProjectConfig{SpokeProject: saved.Credential.SpokeProjectName}, removeProvider: &saved,
		})
		r.states = append(r.states, reconciliationState{})
	}
	return nil
}

func (r *Reconciler) providerMappingRetained(ctx context.Context, saved config.FederationManagedCredentialReservation) (bool, error) {
	for _, target := range r.targets {
		if target.removeProvider != nil {
			continue
		}
		project, err := r.store.ProjectByNameIncludingArchived(ctx, target.Mapping.SpokeProject)
		if errors.Is(err, db.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, reconcileError(ErrLocalStorage, "read configured provider project")
		}
		if project.UID == saved.ProjectUID || project.UID == saved.Credential.Provider.LocalProjectUID {
			return true, nil
		}
	}
	return false, nil
}

func reconcileProviderLeave(
	ctx context.Context, store db.Storage, credentials config.FederationCredentialStore,
	projectName string, removed *config.FederationManagedCredentialReservation,
) (bool, error) {
	managed, ok := credentials.(config.FederationManagedCredentialStore)
	if !ok {
		return false, nil
	}
	var saved config.FederationManagedCredentialReservation
	var found bool
	var err error
	if removed != nil {
		saved, found, err = config.FindProjectManagedCredential(ctx, managed, removed.Credential.Provider.LocalProjectUID, removed.Credential.SpokeProjectName)
		if err == nil && found && (saved.Credential.Provider == nil || saved.Credential.Provider.RequestID != removed.Credential.Provider.RequestID) {
			return true, providerReconciliationError(config.ErrFederationCredentialConflict)
		}
	} else {
		project, projectErr := store.ProjectByNameIncludingArchived(ctx, projectName)
		if errors.Is(projectErr, db.ErrNotFound) {
			return false, nil
		}
		if projectErr != nil {
			return true, reconcileError(ErrLocalStorage, "read configured provider project")
		}
		saved, found, err = config.FindProjectManagedCredential(ctx, managed, project.UID, project.Name)
	}
	if err != nil {
		return true, providerReconciliationError(err)
	}
	if !found || saved.Credential.Provider == nil || (removed == nil && !saved.Credential.LeavePending) {
		return false, nil
	}
	closed, project, err := daemon.ReleaseRemovedFederationProvider(ctx, store, managed, saved)
	if err != nil {
		return true, providerReconciliationError(err)
	}
	if removed != nil {
		if project.ID != 0 {
			if _, err := daemon.LeaveFederationReplica(ctx, store, managed, nil, project.ID); err != nil {
				return true, providerReconciliationError(err)
			}
		}
		if err := managed.DeleteManagedFederationCredential(ctx, closed); err != nil {
			return true, providerReconciliationError(err)
		}
	}
	return true, nil
}

func providerReconciliationError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, federationprovider.ErrInvalidRequest):
		return reconcileError(ErrConfigurationConflict, "federation provider rejected its input; check the helper command and protocol version")
	case errors.Is(err, config.ErrFederationCredentialConflict), errors.Is(err, daemon.ErrFederationReplicaCredentialConflict):
		return reconcileError(ErrConfigurationConflict, "federation provider operation conflicts with local state")
	case errors.Is(err, db.ErrFederationProjectNotEmpty), errors.Is(err, daemon.ErrFederationReplicaBindingConflict):
		return reconcileError(ErrBindingConflict, "federation attachment requires an empty project or explicit migration approval")
	case errors.Is(err, daemon.ErrFederationReplicaCredentialIO):
		return reconcileError(ErrCredentialIO, "save federation provider operation")
	case errors.Is(err, daemon.ErrFederationProviderStorage):
		return reconcileError(ErrLocalStorage, "read retained provider project")
	default:
		return reconcileError(ErrHubUnavailable, "federation provider operation did not complete")
	}
}
