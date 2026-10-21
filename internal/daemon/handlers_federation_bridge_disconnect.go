package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/pkg/client/generated"
)

var errFederationBridgePendingProjectArchived = errors.New("pending bridge setup belongs to an archived project")

func registerFederationBridgeDisconnect(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "disconnectFederationBridge", Method: http.MethodPost, Path: "/api/v1/federation/bridges/{project_name}/disconnect", Summary: "Revoke a selected bridge grant and detach its local replica"}, func(ctx context.Context, in *api.DisconnectFederationBridgeRequest) (*api.DisconnectFederationBridgeResponse, error) {
		if config.ValidateProjectName(in.ProjectName) != nil {
			return nil, api.NewError(400, "validation", "valid local project name required", "", nil)
		}
		credentials := cfg.federationCredentialStore()
		replacer, replaceOK := credentials.(config.FederationCredentialReplacer)
		remover, removeOK := credentials.(config.FederationCredentialRemover)
		if !replaceOK || !removeOK {
			return nil, api.NewError(503, "federation_credentials_unavailable", "exact bridge credential cleanup unavailable", "", nil)
		}
		target, done, err := resolveFederationBridgeDisconnect(ctx, cfg.DB, credentials, in.ProjectName, in.Body.LocalOnly)
		if err != nil {
			return nil, err
		}
		if done {
			// A retry after a lost success response repeats what the operator
			// asked for: a local-only detach still leaves the hub grant to revoke.
			result := api.FederationBridgeDisconnectResult{ProjectName: in.ProjectName, ProjectUID: target.projectUID, Status: "disconnected"}
			if in.Body.LocalOnly {
				result.LocalOnly, result.Warning = true, federationBridgeLocalOnlyWarning
			}
			return &api.DisconnectFederationBridgeResponse{Body: result}, nil
		}
		if in.Body.LocalOnly {
			return disconnectFederationBridgeLocally(ctx, cfg, in.ProjectName, in.Body.Preflight, target)
		}
		project, projectUID, credential, hubURL := target.project, target.projectUID, target.credential, target.hubURL
		client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if err := (config.BearerPolicy{AllowInsecurePlaintext: credential.AllowInsecure}).ConfigureClient(client, hubURL, credential.Token); err != nil {
			return nil, api.NewError(400, "validation", "bridge credential target violates transport policy", "", nil)
		}
		if credential.Signing != nil {
			if err := federationsigning.ConfigureClient(client, hubURL, credential.Token, *credential.Signing); err != nil {
				return nil, api.NewError(http.StatusServiceUnavailable, "federation_signing_unavailable",
					"saved bridge signing credential is unavailable", "restore its configured key source before retrying disconnect", nil)
			}
		}
		validate := func() error {
			current, found, err := credentials.FederationCredential(ctx, projectUID)
			if err != nil {
				return credentialIOError("read bridge credential before disconnect")
			}
			if !found || !current.Equal(credential) {
				return config.ErrFederationCredentialConflict
			}
			currentProject, err := cfg.DB.ProjectByUID(ctx, projectUID)
			if errors.Is(err, db.ErrNotFound) && credential.RelayEnrollmentPending {
				project = db.Project{}
				return nil
			}
			if err != nil {
				return err
			}
			if currentProject.Name != in.ProjectName {
				return ErrFederationReplicaBindingConflict
			}
			if credential.RelayEnrollmentPending && currentProject.DeletedAt != nil {
				return errFederationBridgePendingProjectArchived
			}
			project = currentProject
			validateLifecycle := func() error {
				lifecycle, ok := cfg.DB.(db.RelayLifecycleStore)
				if !ok {
					return db.ErrTransactionFinalizationFailed
				}
				return lifecycle.ValidateRelayLifecycle(ctx, project.ID)
			}
			if credential.RelayEnrollmentPending {
				if err := validateLifecycle(); err != nil {
					return err
				}
			}
			binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
			if errors.Is(err, db.ErrNotFound) && (credential.LeavePending || credential.RelayEnrollmentPending) {
				return nil
			}
			if err != nil {
				return err
			}
			if binding.Role != db.FederationRoleSpoke || (binding.RelayConfig == nil && !credential.RelayEnrollmentPending) || binding.HubURL != hubURL || binding.HubProjectID != credential.HubProjectID {
				return ErrFederationReplicaBindingConflict
			}
			if credential.RelayEnrollmentPending && (binding.HubProjectUID != projectUID || strings.TrimSpace(binding.Actor) != strings.TrimSpace(credential.Actor)) {
				return ErrFederationReplicaBindingConflict
			}
			if !credential.RelayEnrollmentPending {
				if err := validateLifecycle(); err != nil {
					return err
				}
			}
			return nil
		}
		ensureFederationReplicaMu.Lock()
		err = validate()
		if err == nil && !in.Body.Preflight && !credential.LeavePending {
			replacement := credential
			replacement.LeavePending = true
			err = replacer.ReplaceFederationCredential(ctx, config.FederationCredentialReplacement{ProjectUID: projectUID, Expected: credential, Replacement: replacement})
			if err == nil {
				credential = replacement
			}
		}
		if err == nil && !in.Body.Preflight {
			federationReplicaTransitions.markLeavePending(federationReplicaTransitionKey(cfg.DB, in.ProjectName), newFederationReplicaPreparation(cfg.DB, project.ID, ""))
		}
		ensureFederationReplicaMu.Unlock()
		if err != nil {
			return nil, federationBridgeDisconnectAPIError(err)
		}
		result := &api.DisconnectFederationBridgeResponse{Body: api.FederationBridgeDisconnectResult{ProjectName: in.ProjectName, ProjectUID: projectUID, Status: "ready"}}
		if in.Body.Preflight {
			return result, nil
		}
		if err := waitFederationReplicaOperations(ctx, federationReplicaTransitionKey(cfg.DB, in.ProjectName)); err != nil {
			return nil, federationBridgeDisconnectAPIError(err)
		}
		if project.ID != 0 {
			finish, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), project.ID), cfg.DB, project.ID)
			if err != nil {
				return nil, federationBridgeDisconnectAPIError(err)
			}
			defer finish()
		}
		// Revalidate after draining and persist a push-disabled leave state before
		// network I/O. Keeping the spoke enabled makes the shared write gate reject
		// ordinary project mutations until the upstream grant has been revoked and
		// local detachment completes. A pending leave remains recoverable on retry.
		ensureFederationReplicaMu.Lock()
		err = validate()
		if err == nil && project.ID != 0 {
			fencer, ok := cfg.DB.(db.RelayDisconnectFenceStore)
			if !ok {
				err = db.ErrTransactionFinalizationFailed
			} else {
				err = fencer.FenceRelayDisconnect(ctx, project.ID)
			}
		}
		ensureFederationReplicaMu.Unlock()
		if err != nil {
			return nil, federationBridgeDisconnectAPIError(err)
		}
		bridge, err := newFederationBridgeAPIClient(client, hubURL)
		if err != nil {
			return nil, internalAPIError(err)
		}
		upstreamResponse, callErr := bridge.DisconnectRelayEnrollmentWithResponse(ctx, &generated.DisconnectRelayEnrollmentRequestOptions{
			PathParams: &generated.DisconnectRelayEnrollmentPath{ProjectID: credential.HubProjectID},
			Body:       &generated.DisconnectRelayEnrollmentBody{SpokeInstanceUID: cfg.DB.InstanceUID()},
		})
		var upstreamRaw []byte
		if upstreamResponse != nil {
			upstreamRaw = upstreamResponse.Body
		}
		var upstream api.DisconnectRelayResponse
		grantResolved := false
		if err := decodeFederationBridgeResponse(upstreamRaw, callErr, &upstream.Body); err != nil {
			hubStatus, ok := federationBridgeHubStatus(err)
			if !credential.RelayEnrollmentPending || !ok || hubStatus != http.StatusNotFound {
				return nil, err
			}
			grantResolved = true
		} else {
			grantResolved = upstream.Body.Revoked
		}
		if !grantResolved {
			return nil, api.NewError(409, "invalid_relay_grant", "hub did not confirm grant revocation", "retry disconnect with the retained credential", nil)
		}
		ensureFederationReplicaMu.Lock()
		err = validate()
		if err == nil && project.ID != 0 {
			_, err = cfg.DB.LeaveFederationReplica(ctx, project.ID)
		}
		if err == nil {
			err = remover.DeleteFederationCredentialIfUnchanged(ctx, projectUID, credential)
		}
		if err == nil {
			federationReplicaTransitions.markLeft(federationReplicaTransitionKey(cfg.DB, in.ProjectName))
		}
		ensureFederationReplicaMu.Unlock()
		if err != nil {
			return nil, federationBridgeDisconnectAPIError(err)
		}
		if cfg.FederationWake != nil {
			cfg.FederationWake()
		}
		result.Body.Status = "disconnected"
		return result, nil
	})
}

// federationBridgeDisconnectTarget is the local project, hub binding and
// retained manual credential that one bridge disconnect acts on. An ordinary
// disconnect always carries the credential; a local-only detach of an
// established bridge carries one only when it is still saved.
type federationBridgeDisconnectTarget struct {
	project         db.Project
	projectUID      string
	credential      config.FederationCredential
	credentialFound bool
	hubURL          string
	hubProjectID    int64
}

// resolveFederationBridgeDisconnect finds the bridge to disconnect by local
// project name, falling back to a pending credential when no project exists.
// A local-only request resolves an established relay bridge from its database
// binding, so a database restored without its credential file can still be
// detached. done reports that nothing remains to disconnect.
func resolveFederationBridgeDisconnect(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, name string, localOnly bool) (federationBridgeDisconnectTarget, bool, error) {
	var target federationBridgeDisconnectTarget
	project, err := store.ProjectByNameIncludingArchived(ctx, name)
	switch {
	case err == nil:
		target.project, target.projectUID = project, project.UID
		if localOnly {
			binding, bindingErr := store.FederationBindingByProject(ctx, project.ID)
			if bindingErr != nil && !errors.Is(bindingErr, db.ErrNotFound) {
				return target, false, internalAPIError(bindingErr)
			}
			// Any spoke binding qualifies, including one left without relay
			// configuration by a hub that vanished while enrollment was pending.
			if bindingErr == nil && binding.Role == db.FederationRoleSpoke {
				return resolveFederationBridgeBindingTarget(ctx, credentials, target, binding)
			}
		}
	case errors.Is(err, db.ErrNotFound):
		reader, ok := credentials.(config.FederationRelayPendingMetadataReader)
		if !ok {
			return target, false, api.NewError(503, "federation_credentials_unavailable", "pending bridge metadata unavailable", "", nil)
		}
		var found bool
		target.projectUID, _, found, err = reader.PendingRelayCredentialMetadata(ctx, name)
		if err != nil {
			return target, false, federationBridgeDisconnectAPIError(err)
		}
		if !found {
			return target, true, nil
		}
	default:
		return target, false, internalAPIError(err)
	}
	credential, found, err := credentials.FederationCredential(ctx, target.projectUID)
	if err != nil {
		return target, false, api.NewError(503, "federation_credentials_unavailable", "cannot read bridge credential", "", nil)
	}
	if !found && target.project.ID != 0 {
		_, bindingErr := store.FederationBindingByProject(ctx, target.project.ID)
		if errors.Is(bindingErr, db.ErrNotFound) {
			return target, true, nil
		}
		if bindingErr != nil {
			return target, false, internalAPIError(bindingErr)
		}
	}
	if !found || credential.Token == "" || credential.Provider != nil || credential.ManagedByConfig || credential.SpokeProjectName != name {
		return target, false, api.NewError(409, "federation_credential_conflict", "selected bridge has no matching manual credential", "", nil)
	}
	hubURL, err := httpurl.CanonicalHTTPOrigin(credential.HubURL)
	if err != nil || strings.TrimRight(credential.HubURL, "/") != hubURL {
		return target, false, api.NewError(409, "federation_credential_conflict", "bridge origin is invalid", "", nil)
	}
	target.credential, target.credentialFound = credential, true
	target.hubURL, target.hubProjectID = hubURL, credential.HubProjectID
	return target, false, nil
}

// resolveFederationBridgeBindingTarget completes a local-only target from the
// project's spoke binding. A saved credential must be this bridge's manual
// credential so the detach can remove it; without one the detach proceeds and
// removes nothing else. Unlike the credential-driven path it does not require
// a token or a matching spoke project name: the detach never presents the
// token to a hub, and the binding, not the credential, names the project.
func resolveFederationBridgeBindingTarget(ctx context.Context, credentials config.FederationCredentialStore, target federationBridgeDisconnectTarget, binding db.FederationBinding) (federationBridgeDisconnectTarget, bool, error) {
	target.hubURL, target.hubProjectID = binding.HubURL, binding.HubProjectID
	credential, found, err := credentials.FederationCredential(ctx, target.projectUID)
	if err != nil {
		return target, false, api.NewError(503, "federation_credentials_unavailable", "cannot read bridge credential", "", nil)
	}
	if !found {
		return target, false, nil
	}
	credentialHubURL, err := httpurl.CanonicalHTTPOrigin(credential.HubURL)
	if err != nil || credential.Provider != nil || credential.ManagedByConfig || credentialHubURL != binding.HubURL || credential.HubProjectID != binding.HubProjectID {
		return target, false, api.NewError(409, "federation_credential_conflict", "saved credential does not match the selected bridge", "resolve the retained credential before retrying", nil)
	}
	target.credential, target.credentialFound = credential, true
	return target, false, nil
}

func federationBridgeDisconnectAPIError(err error) error {
	if errors.Is(err, errFederationBridgePendingProjectArchived) {
		return api.NewError(http.StatusConflict, "federation_lifecycle_blocked", "archived project cannot be detached while bridge setup is pending", "restore the project before retrying disconnect", nil)
	}
	if errors.Is(err, config.ErrFederationCredentialConflict) {
		return api.NewError(409, "federation_credential_conflict", "bridge credential changed during disconnect", "resolve the retained credential before retrying", nil)
	}
	if errors.Is(err, ErrFederationReplicaBindingConflict) {
		return api.NewError(409, "federation_binding_conflict", "bridge binding changed during disconnect", "retry after resolving the current binding", nil)
	}
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(404, "not_found", "bridge no longer exists", "", nil)
	}
	return federationReplicaAPIError(err)
}
