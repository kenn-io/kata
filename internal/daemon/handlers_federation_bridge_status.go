package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func registerFederationBridgeStatus(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "getFederationBridgeStatus", Method: http.MethodGet, Path: "/api/v1/federation/bridges/{project_name}", Summary: "Read one local bridge's recorded connection state"}, func(ctx context.Context, in *api.FederationBridgeStatusRequest) (*api.FederationBridgeStatusResponse, error) {
		if err := config.ValidateProjectName(in.ProjectName); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		project, err := cfg.DB.ProjectByName(ctx, in.ProjectName)
		if errors.Is(err, db.ErrNotFound) {
			return pendingFederationBridgeStatus(ctx, cfg, in.ProjectName)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && binding.RelayConfig == nil) {
			credential, found, credentialErr := cfg.federationCredentialStore().FederationCredential(
				ctx, project.UID,
			)
			if credentialErr != nil {
				return nil, api.NewError(
					http.StatusServiceUnavailable, "federation_credentials_unavailable",
					"cannot read the bridge credential status", "", nil,
				)
			}
			pending := found && credential.RelayEnrollmentPending && credential.SpokeProjectName == project.Name
			if err == nil {
				pending = pending && binding.Role == db.FederationRoleSpoke &&
					credential.HubURL == binding.HubURL && credential.HubProjectID == binding.HubProjectID &&
					credential.Actor == binding.Actor
			}
			if pending {
				metadata := credential.Metadata()
				return &api.FederationBridgeStatusResponse{Body: api.FederationBridgeStatusBody{
					ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name,
					HubURL: metadata.HubURL, HubCatalog: metadata.HubCatalog,
					LocalAccount: metadata.RequestedActor, UpstreamAccount: metadata.Actor,
					Direction: "bidirectional", State: "enrollment_pending",
					CredentialStatus: metadata.Status,
				}}, nil
			}
			return nil, api.NewError(404, "not_found", "bridge not found", "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
		if err != nil {
			return nil, api.NewError(503, "federation_credentials_unavailable", "cannot read the bridge credential status", "", nil)
		}
		metadata := credential.Metadata()
		status, err := federationProjectStatus(ctx, cfg.DB, cfg.federationCredentialStore(), nil, binding, false)
		if err != nil {
			return nil, err
		}
		state := "connected"
		switch {
		case binding.RelayConfig.UpstreamRevoked:
			state = "revoked"
		case credential.RelayEnrollmentPending:
			state = "enrollment_pending"
		case !binding.Enabled:
			state = "paused"
		case !found || credential.Token == "" || credential.LeavePending || (status.LastErrorAt != nil && (status.LastSuccessfulSyncAt == nil || status.LastErrorAt.After(*status.LastSuccessfulSyncAt))):
			state = "offline"
		}
		direction := "bidirectional"
		if !binding.PushEnabled || !federationCapabilitiesContain(credential.Capabilities, "push") {
			direction = "pull_only"
		}
		return &api.FederationBridgeStatusResponse{Body: api.FederationBridgeStatusBody{ProjectID: project.ID, ProjectUID: project.UID, ProjectName: project.Name, HubURL: binding.HubURL, HubCatalog: metadata.HubCatalog, LocalAccount: binding.RelayConfig.LocalActor, UpstreamAccount: binding.Actor, Direction: direction, State: state, CredentialStatus: metadata.Status, Relay: binding.RelayConfig, Federation: &status}}, nil
	})
}

func pendingFederationBridgeStatus(ctx context.Context, cfg ServerConfig, name string) (*api.FederationBridgeStatusResponse, error) {
	reader, ok := cfg.federationCredentialStore().(config.FederationRelayPendingMetadataReader)
	if !ok {
		return nil, api.NewError(503, "federation_credentials_unavailable", "pending bridge metadata is unavailable", "", nil)
	}
	uid, metadata, found, err := reader.PendingRelayCredentialMetadata(ctx, name)
	if errors.Is(err, config.ErrFederationCredentialConflict) {
		return nil, api.NewError(409, "federation_credential_conflict", "pending bridge project name is ambiguous", "resolve the retained enrollment candidates before retrying", nil)
	}
	if err != nil {
		return nil, api.NewError(503, "federation_credentials_unavailable", "cannot read pending bridge metadata", "", nil)
	}
	if !found {
		return nil, api.NewError(404, "not_found", "bridge not found", "", nil)
	}
	return &api.FederationBridgeStatusResponse{Body: api.FederationBridgeStatusBody{ProjectUID: uid, ProjectName: name, HubURL: metadata.HubURL, HubCatalog: metadata.HubCatalog, LocalAccount: metadata.RequestedActor, UpstreamAccount: metadata.Actor, Direction: "bidirectional", State: "enrollment_pending", CredentialStatus: metadata.Status}}, nil
}
