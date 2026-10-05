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
		projectUID := ""
		project, err := cfg.DB.ProjectByNameIncludingArchived(ctx, in.ProjectName)
		if err == nil {
			projectUID = project.UID
		} else if errors.Is(err, db.ErrNotFound) {
			reader, ok := credentials.(config.FederationRelayPendingMetadataReader)
			if !ok {
				return nil, api.NewError(503, "federation_credentials_unavailable", "pending bridge metadata unavailable", "", nil)
			}
			var found bool
			projectUID, _, found, err = reader.PendingRelayCredentialMetadata(ctx, in.ProjectName)
			if err != nil {
				return nil, federationBridgeDisconnectAPIError(err)
			}
			if !found {
				return &api.DisconnectFederationBridgeResponse{Body: api.FederationBridgeDisconnectResult{ProjectName: in.ProjectName, Status: "disconnected"}}, nil
			}
		} else {
			return nil, internalAPIError(err)
		}
		credential, found, err := credentials.FederationCredential(ctx, projectUID)
		if err != nil {
			return nil, api.NewError(503, "federation_credentials_unavailable", "cannot read bridge credential", "", nil)
		}
		if !found && project.ID != 0 {
			_, bindingErr := cfg.DB.FederationBindingByProject(ctx, project.ID)
			if errors.Is(bindingErr, db.ErrNotFound) {
				return &api.DisconnectFederationBridgeResponse{Body: api.FederationBridgeDisconnectResult{ProjectName: in.ProjectName, ProjectUID: projectUID, Status: "disconnected"}}, nil
			}
			if bindingErr != nil {
				return nil, internalAPIError(bindingErr)
			}
		}
		if !found || credential.Token == "" || credential.Provider != nil || credential.ManagedByConfig || credential.SpokeProjectName != in.ProjectName {
			return nil, api.NewError(409, "federation_credential_conflict", "selected bridge has no matching manual credential", "", nil)
		}
		hubURL, err := httpurl.CanonicalHTTPOrigin(credential.HubURL)
		if err != nil || strings.TrimRight(credential.HubURL, "/") != hubURL {
			return nil, api.NewError(409, "federation_credential_conflict", "bridge origin is invalid", "", nil)
		}
		client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if err := (config.BearerPolicy{AllowInsecurePlaintext: credential.AllowInsecure}).ConfigureClient(client, hubURL, credential.Token); err != nil {
			return nil, api.NewError(400, "validation", "bridge credential target violates transport policy", "", nil)
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
			federationReplicaTransitions.markLeavePending(federationReplicaTransitionKey(cfg.DB, in.ProjectName))
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
		// Revalidate after draining and stop transport durably before network I/O.
		ensureFederationReplicaMu.Lock()
		err = validate()
		if err == nil && project.ID != 0 {
			binding, readErr := cfg.DB.FederationBindingByProject(ctx, project.ID)
			if readErr == nil && binding.Enabled {
				binding.Enabled = false
				_, err = cfg.DB.UpsertFederationBinding(ctx, binding)
			} else if readErr != nil && !errors.Is(readErr, db.ErrNotFound) {
				err = readErr
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
			apiErr, ok := errors.AsType[*api.APIError](err)
			if !credential.RelayEnrollmentPending || !ok || apiErr.Status != http.StatusNotFound {
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
