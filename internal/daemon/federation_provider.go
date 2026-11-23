package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/kata/internal/httpurl"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/pkg/federationprovider"
)

// ErrFederationProviderStorage distinguishes local database failure from a
// failed exchange with the credential provider.
var ErrFederationProviderStorage = errors.New("federation provider local storage failed")

// ErrFederationProviderIntentChangeBlocked reports an intent change that
// waits until the hub accepts this replica's local changes, the replica holds
// no claims or claim requests, and no cron run of the bound actor is still
// running. Nothing changes and no local data is dropped while it is returned.
var ErrFederationProviderIntentChangeBlocked = errors.New("federation provider intent change waits for local work")

// AuthorizeFederationProvider saves a candidate before contacting the trusted
// helper. A ready result is retained before the caller may fetch metadata or
// bind the replica. This operation does not itself enable federation.
func AuthorizeFederationProvider(
	ctx context.Context, store db.Storage, credentials config.FederationCredentialStore,
	catalog config.CatalogDaemonConfig, mapping config.FederationProjectConfig,
) (config.FederationManagedCredentialReservation, error) {
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return config.FederationManagedCredentialReservation{}, err
	}
	reservation, changeIntent, err := prepareFederationProvider(ctx, store, managed, catalog, mapping)
	if err != nil {
		return reservation, err
	}
	if changeIntent {
		reservation, err = changeFederationProviderIntent(ctx, store, managed, mapping, reservation)
		if err != nil {
			return reservation, err
		}
	}
	switch reservation.Credential.Provider.Status {
	case federationprovider.StatusReady, federationprovider.StatusDenied, federationprovider.StatusConflict:
		return reservation, nil
	}
	finish, err := BeginFederationReplicaHubOperation(ctx, store, managed, mapping.SpokeProject, reservation)
	if err != nil {
		return reservation, err
	}
	result, exchangeErr := exchangeFederationProvider(ctx, managed, reservation)
	// Release uses the saved request ID, never a response-supplied enrollment
	// ID. The existing drain still prevents leave from deleting in-flight state.
	leavePending, finishErr := finish(ctx, 0)
	if finishErr != nil {
		return reservation, finishErr
	}
	if leavePending {
		return reservation, ErrFederationReplicaLeavePending
	}
	return result, exchangeErr
}

// prepareFederationProvider also reports whether the saved request differs
// from the mapping only by an intent change that a bound replica can make in
// place. The caller then releases the saved request before authorizing anew.
func prepareFederationProvider(
	ctx context.Context, store db.Storage, managed config.FederationManagedCredentialStore,
	catalog config.CatalogDaemonConfig, mapping config.FederationProjectConfig,
) (config.FederationManagedCredentialReservation, bool, error) {
	var empty config.FederationManagedCredentialReservation
	if len(mapping.CredentialProvider) == 0 || mapping.Hub != catalog.Name || catalog.Local ||
		config.ValidateFederationAuthentication(mapping, catalog) != nil ||
		config.ValidateProjectName(mapping.SpokeProject) != nil || config.ValidateProjectName(mapping.HubProject) != nil {
		return empty, false, ErrFederationReplicaInvalidInput
	}
	base, err := httpurl.CanonicalHTTPBaseURL(catalog.URL)
	if err != nil {
		return empty, false, ErrFederationReplicaInvalidInput
	}
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	if err := federationReplicaTransitions.leaveBlockedError(federationReplicaTransitionKey(store, mapping.SpokeProject)); err != nil {
		return empty, false, err
	}
	project, err := store.ProjectByName(ctx, mapping.SpokeProject)
	if err != nil {
		return empty, false, err
	}
	previous, found, err := config.FindProjectManagedCredential(ctx, managed, project.UID, project.Name)
	if err != nil {
		return empty, false, err
	}
	if found {
		reservation, err := matchFederationProvider(previous, project, store.InstanceUID(), base, catalog, mapping)
		if !releaseUnconfirmed(previous, mapping) &&
			(!errors.Is(err, ErrFederationReplicaCredentialConflict) || !intentChangeAllowed(previous, mapping)) {
			return reservation, false, err
		}
		kept := mapping
		kept.Intent = previous.Credential.Provider.Intent
		if _, err := matchFederationProvider(previous, project, store.InstanceUID(), base, catalog, kept); err != nil {
			return previous, false, err
		}
		if _, err := requireBoundProviderReplica(ctx, store, project, previous); err != nil {
			return previous, false, err
		}
		return previous, true, nil
	}
	if _, found, err := managed.FederationCredential(ctx, project.UID); err != nil {
		return empty, false, err
	} else if found {
		return empty, false, ErrFederationReplicaCredentialConflict
	}
	if _, err := store.FederationBindingByProject(ctx, project.ID); !errors.Is(err, db.ErrNotFound) {
		if err != nil {
			return empty, false, err
		}
		return empty, false, ErrFederationReplicaBindingConflict
	}
	token, err := db.NewFederationToken()
	if err != nil {
		return empty, false, err
	}
	reservation := config.FederationManagedCredentialReservation{ProjectUID: project.UID, Credential: config.FederationCredential{
		HubURL: base, Token: token, ManagedByConfig: true,
		HubCatalog: catalog.Name, HubProjectName: mapping.HubProject, SpokeProjectName: mapping.SpokeProject,
		Provider: &config.FederationProviderCredential{
			RequestID: uuid.New(), Command: slices.Clone(mapping.CredentialProvider), Intent: mapping.Intent,
			SpokeInstanceUID: store.InstanceUID(), LocalProjectUID: project.UID,
		},
	}}
	if err := managed.ReserveManagedFederationCredential(ctx, reservation); err != nil {
		return empty, false, err
	}
	return reservation, false, nil
}

// Switching between read-only and collaborating access keeps the replica's
// tasks and history. Adoption and other identity changes still need an
// explicit leave.
func intentChangeAllowed(saved config.FederationManagedCredentialReservation, mapping config.FederationProjectConfig) bool {
	p := saved.Credential.Provider
	return p != nil && p.Intent != mapping.Intent && intentSwitchable(p.Intent) && intentSwitchable(mapping.Intent)
}

// releaseUnconfirmed reports a saved request whose release was sent without
// an answer. The provider may have revoked it, so it is replaced even when
// the mapping restored the request's intent.
func releaseUnconfirmed(saved config.FederationManagedCredentialReservation, mapping config.FederationProjectConfig) bool {
	p := saved.Credential.Provider
	return p != nil && p.ReleaseStarted && intentSwitchable(p.Intent) && intentSwitchable(mapping.Intent)
}

func intentSwitchable(intent federationprovider.Intent) bool {
	return intent == federationprovider.IntentReadOnly || intent == federationprovider.IntentCollaborate
}

// An unbound request has no local replica to keep, so its intent still
// conflicts. The binding must already follow the approved hub project.
func requireBoundProviderReplica(ctx context.Context, store db.Storage, project db.Project, saved config.FederationManagedCredentialReservation) (db.FederationBinding, error) {
	hubProjectUID := saved.Credential.Provider.HubProjectUID
	binding, err := store.FederationBindingByProject(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return binding, ErrFederationReplicaCredentialConflict
	}
	if err != nil {
		return binding, err
	}
	if hubProjectUID == "" || project.UID != hubProjectUID || binding.Role != db.FederationRoleSpoke ||
		binding.RelayConfig != nil || binding.HubProjectUID != hubProjectUID ||
		binding.HubProjectID != saved.Credential.HubProjectID || strings.TrimSpace(binding.Actor) == "" {
		return binding, ErrFederationReplicaBindingConflict
	}
	// Sync and claims send the credential to the binding's address. A rebind
	// that saved the new address only in the credential must finish first,
	// or a token approved for one address would go to the other.
	if bindingBase, err := httpurl.CanonicalHTTPBaseURL(binding.HubURL); err != nil || bindingBase != saved.Credential.HubURL {
		return binding, ErrFederationReplicaBindingConflict
	}
	return binding, nil
}

// changeFederationProviderIntent releases the saved request, then saves a new
// request with the mapping's intent for the same local and hub projects. It
// never detaches the binding or deletes local data. Lowering the intent first
// stops pushing, which it does only once the hub accepted every local change
// and the replica holds no claims. A failure before release confirmation
// keeps the saved request, so a retry repeats the same release.
func changeFederationProviderIntent(
	ctx context.Context, store db.Storage, managed config.FederationManagedCredentialStore,
	mapping config.FederationProjectConfig, saved config.FederationManagedCredentialReservation,
) (config.FederationManagedCredentialReservation, error) {
	project, err := store.ProjectByName(ctx, mapping.SpokeProject)
	if err != nil {
		return saved, errors.Join(ErrFederationProviderStorage, err)
	}
	// A claim request already sent with the saved credential may be granted
	// by the hub before its response is cached. Wait for it, and keep new
	// ones out, until the saved request is replaced; otherwise the release
	// could revoke the only credential able to release that claim.
	finishTransport, err := federationcoord.BeginRebind(ctx, federationcoord.Key(store.InstanceUID(), project.ID), store, project.ID)
	if err != nil {
		return saved, err
	}
	defer finishTransport()
	finish, err := BeginFederationReplicaHubOperation(ctx, store, managed, mapping.SpokeProject, saved)
	if err != nil {
		return saved, err
	}
	saved, response, releaseErr := releaseForIntentChange(ctx, store, managed, mapping, saved)
	leavePending, finishErr := finish(ctx, 0)
	if finishErr != nil {
		return saved, finishErr
	}
	if leavePending {
		return saved, ErrFederationReplicaLeavePending
	}
	if releaseErr != nil {
		return saved, releaseErr
	}
	if response.Status != federationprovider.StatusReleased {
		return saved, errors.New("federation provider release is not confirmed")
	}
	// The new approval may name a new actor. Record the actor the binding
	// holds now so only that actor moves to the new one. The binding still
	// holds it after an earlier attempt that never applied.
	project, err = store.ProjectByName(ctx, mapping.SpokeProject)
	if err != nil {
		return saved, errors.Join(ErrFederationProviderStorage, err)
	}
	binding, err := requireBoundProviderReplica(ctx, store, project, saved)
	if err != nil {
		return saved, err
	}
	token, err := db.NewFederationToken()
	if err != nil {
		return saved, err
	}
	p := saved.Credential.Provider
	next := *p
	next.RequestID = uuid.New()
	next.Intent = mapping.Intent
	next.Status = ""
	next.EnrollmentID = 0
	next.ExpiresAt = time.Time{}
	next.PreviousActor = strings.TrimSpace(binding.Actor)
	next.ReleaseStarted = false
	replacement := saved
	replacement.Credential.Token = token
	replacement.Credential.Provider = &next
	if err := managed.ReplaceManagedFederationCredential(ctx, saved, replacement); err != nil {
		return saved, err
	}
	return replacement, nil
}

// releaseForIntentChange returns the saved request as it now stands: marked
// while the provider's answer is unknown, unmarked once the provider refused
// the release and so still holds the approval. An unavailable provider has
// not answered either way, so the mark stays.
func releaseForIntentChange(
	ctx context.Context, store db.Storage, managed config.FederationManagedCredentialStore,
	mapping config.FederationProjectConfig, saved config.FederationManagedCredentialReservation,
) (config.FederationManagedCredentialReservation, federationprovider.Response, error) {
	// A collaborate approval can hold claims. Releasing it, for a downgrade
	// or when a restored intent repeats an unanswered release, waits until
	// no claim depends on it. Push is already off on a repeat.
	if saved.Credential.Provider.Intent == federationprovider.IntentCollaborate {
		project, err := store.ProjectByName(ctx, mapping.SpokeProject)
		if err != nil {
			return saved, federationprovider.Response{}, errors.Join(ErrFederationProviderStorage, err)
		}
		if _, err := store.DisableFederationPushWhenSettled(ctx, project.ID, store.InstanceUID()); err != nil {
			if federationReplicaWorkOpen(err) {
				return saved, federationprovider.Response{}, errors.Join(ErrFederationProviderIntentChangeBlocked, err)
			}
			if errors.Is(err, db.ErrExternalRootFederationConflict) {
				return saved, federationprovider.Response{}, errors.Join(ErrFederationReplicaBindingConflict, err)
			}
			return saved, federationprovider.Response{}, errors.Join(ErrFederationProviderStorage, err)
		}
	}
	marked, err := withReleaseStarted(ctx, managed, saved, true)
	if err != nil {
		return saved, federationprovider.Response{}, err
	}
	p := marked.Credential.Provider
	response, err := federationprovider.Exchange(ctx, p.Command, federationprovider.Request{
		Version: 1, Operation: "release", RequestID: p.RequestID,
		HubURL: marked.Credential.HubURL, Project: marked.Credential.HubProjectName,
		SpokeInstanceUID: p.SpokeInstanceUID, LocalProjectUID: p.LocalProjectUID, Intent: p.Intent,
	})
	if err != nil || (response.Status != federationprovider.StatusDenied && response.Status != federationprovider.StatusConflict) {
		return marked, response, err
	}
	unmarked, err := withReleaseStarted(ctx, managed, marked, false)
	if err != nil {
		return marked, response, err
	}
	return unmarked, response, nil
}

func withReleaseStarted(
	ctx context.Context, managed config.FederationManagedCredentialStore,
	saved config.FederationManagedCredentialReservation, started bool,
) (config.FederationManagedCredentialReservation, error) {
	if saved.Credential.Provider.ReleaseStarted == started {
		return saved, nil
	}
	next := *saved.Credential.Provider
	next.ReleaseStarted = started
	replacement := saved
	replacement.Credential.Provider = &next
	if err := managed.ReplaceManagedFederationCredential(ctx, saved, replacement); err != nil {
		return saved, err
	}
	return replacement, nil
}

func matchFederationProvider(
	reservation config.FederationManagedCredentialReservation, project db.Project,
	instance, base string, catalog config.CatalogDaemonConfig, mapping config.FederationProjectConfig,
) (config.FederationManagedCredentialReservation, error) {
	c := reservation.Credential
	p := c.Provider
	if p == nil || c.HubURL != base || c.HubCatalog != catalog.Name || c.HubProjectName != mapping.HubProject ||
		p.Intent != mapping.Intent || p.SpokeInstanceUID != instance ||
		!slices.Equal(p.Command, mapping.CredentialProvider) ||
		(project.UID != p.LocalProjectUID && project.UID != p.HubProjectUID) ||
		(reservation.ProjectUID != p.LocalProjectUID && reservation.ProjectUID != p.HubProjectUID) {
		return reservation, ErrFederationReplicaCredentialConflict
	}
	if c.LeavePending || p.Status == federationprovider.StatusReleased {
		return reservation, ErrFederationReplicaLeavePending
	}
	return reservation, nil
}

func exchangeFederationProvider(
	ctx context.Context, managed config.FederationManagedCredentialStore,
	reservation config.FederationManagedCredentialReservation,
) (config.FederationManagedCredentialReservation, error) {
	c := reservation.Credential
	p := c.Provider
	request := federationprovider.Request{
		Version: 1, Operation: "authorize", RequestID: p.RequestID,
		HubURL: c.HubURL, Project: c.HubProjectName, SpokeInstanceUID: p.SpokeInstanceUID,
		LocalProjectUID: p.LocalProjectUID, Intent: p.Intent, CandidateToken: c.Token,
	}
	response, err := federationprovider.Exchange(ctx, p.Command, request)
	if err != nil {
		return reservation, err
	}
	replacement := reservation
	next := *p
	next.Status = response.Status
	replacement.Credential.Provider = &next
	if response.Status == federationprovider.StatusReady {
		response.Actor = strings.TrimSpace(response.Actor)
		if db.ValidateTokenActor(response.Actor) != nil {
			return reservation, federationprovider.ErrInvalidResponse
		}
		// A request that replaced an earlier approval for a bound replica must
		// approve the same hub project. Keep it unready rather than rebinding.
		if p.HubProjectUID != "" && (response.ProjectUID != p.HubProjectUID || response.ProjectID != c.HubProjectID) {
			return reservation, ErrFederationReplicaBindingConflict
		}
		replacement.Credential.HubProjectID = response.ProjectID
		replacement.Credential.Actor = response.Actor
		replacement.Credential.Capabilities = response.Capabilities
		next.HubProjectUID = response.ProjectUID
		next.EnrollmentID = response.EnrollmentID
		next.ExpiresAt = response.ExpiresAt
	}
	// This exact replacement also refuses an intervening leave marker. Keeping
	// the earlier candidate is sufficient to release even if this write fails.
	if err := managed.ReplaceManagedFederationCredential(ctx, reservation, replacement); err != nil {
		return reservation, err
	}
	return replacement, nil
}

// ReleaseFederationProvider fences local provider work before requesting exact
// release. It retains the credential and request until confirmed, including
// when the helper is offline. The caller may detach after status released.
func ReleaseFederationProvider(
	ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, projectID int64,
) (config.FederationManagedCredentialReservation, error) {
	managed, err := managedCredentialStore(credentials)
	if err != nil {
		return config.FederationManagedCredentialReservation{}, err
	}
	project, err := store.ProjectByID(ctx, projectID)
	if err != nil {
		return config.FederationManagedCredentialReservation{}, err
	}
	return releaseFederationProvider(ctx, store, managed, project, uuid.UUID{})
}

// ReleaseRemovedFederationProvider resolves retained identity even when the
// project's local name changed or its unbound row was purged.
func ReleaseRemovedFederationProvider(ctx context.Context, store db.Storage, managed config.FederationManagedCredentialStore, saved config.FederationManagedCredentialReservation) (config.FederationManagedCredentialReservation, db.Project, error) {
	if saved.Credential.Provider == nil {
		return saved, db.Project{}, ErrFederationReplicaCredentialConflict
	}
	project, err := store.ProjectByUID(ctx, saved.ProjectUID)
	if errors.Is(err, db.ErrNotFound) {
		project, err = store.ProjectByUID(ctx, saved.Credential.Provider.LocalProjectUID)
	}
	if errors.Is(err, db.ErrNotFound) {
		project = db.Project{UID: saved.ProjectUID, Name: saved.Credential.SpokeProjectName}
	} else if err != nil {
		return saved, project, errors.Join(ErrFederationProviderStorage, err)
	}
	closed, err := releaseFederationProvider(ctx, store, managed, project, saved.Credential.Provider.RequestID)
	return closed, project, err
}

func releaseFederationProvider(ctx context.Context, store db.Storage, managed config.FederationManagedCredentialStore, project db.Project, expectedRequest uuid.UUID) (config.FederationManagedCredentialReservation, error) {
	previous, found, err := config.FindProjectManagedCredential(ctx, managed, project.UID, project.Name)
	if err != nil {
		return previous, err
	}
	if !found || previous.Credential.Provider == nil || (expectedRequest != (uuid.UUID{}) && previous.Credential.Provider.RequestID != expectedRequest) {
		return previous, ErrFederationReplicaCredentialConflict
	}
	prepared, err := prepareFederationReplicaLeave(ctx, store, managed, project, "", false)
	if err != nil {
		return previous, err
	}
	reservation := prepared.ManagedReservation
	p := reservation.Credential.Provider
	if !prepared.ManagedReservationFound || p == nil || p.RequestID != previous.Credential.Provider.RequestID {
		return previous, ErrFederationReplicaCredentialConflict
	}
	if p.Status == federationprovider.StatusReleased {
		return reservation, nil
	}
	response, err := federationprovider.Exchange(ctx, p.Command, federationprovider.Request{
		Version: 1, Operation: "release", RequestID: p.RequestID,
		HubURL: reservation.Credential.HubURL, Project: reservation.Credential.HubProjectName,
		SpokeInstanceUID: p.SpokeInstanceUID, LocalProjectUID: p.LocalProjectUID, Intent: p.Intent,
	})
	if err != nil {
		return reservation, err
	}
	if response.Status != federationprovider.StatusReleased {
		return reservation, errors.New("federation provider release is not confirmed")
	}
	replacement := reservation
	next := *p
	next.Status = federationprovider.StatusReleased
	replacement.Credential.Provider = &next
	replacement.Credential.Token = ""
	if err := managed.ReplaceManagedFederationCredential(ctx, reservation, replacement); err != nil {
		return reservation, err
	}
	return replacement, nil
}
