package daemon

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

const federationBridgeLocalOnlyWarning = "the hub grant was not revoked; ask the hub operator to revoke this bridge's relay enrollment"

// disconnectFederationBridgeLocally detaches a bridge whose hub is gone for
// good. It never contacts the hub: it discards undeliverable relay state,
// removes the binding and any retained credential, and keeps the project. The
// target's database binding is the authority; a saved credential is removed
// with it but is not required.
func disconnectFederationBridgeLocally(ctx context.Context, cfg ServerConfig, name string, preflight bool, target federationBridgeDisconnectTarget) (*api.DisconnectFederationBridgeResponse, error) {
	credentials := cfg.federationCredentialStore()
	replacer, replaceOK := credentials.(config.FederationCredentialReplacer)
	remover, removeOK := credentials.(config.FederationCredentialRemover)
	detacher, detachOK := cfg.DB.(db.RelayLocalDetacher)
	if !replaceOK || !removeOK || !detachOK {
		return nil, api.NewError(http.StatusServiceUnavailable, "federation_credentials_unavailable", "local bridge cleanup unavailable", "", nil)
	}
	credential, credentialFound, project := target.credential, target.credentialFound, target.project
	validate := func() error {
		current, found, err := credentials.FederationCredential(ctx, target.projectUID)
		if err != nil {
			return credentialIOError("read bridge credential before local disconnect")
		}
		if found != credentialFound || (found && !current.Equal(credential)) {
			return config.ErrFederationCredentialConflict
		}
		enrollmentPending := credentialFound && credential.RelayEnrollmentPending
		currentProject, err := cfg.DB.ProjectByUID(ctx, target.projectUID)
		if errors.Is(err, db.ErrNotFound) && enrollmentPending {
			project = db.Project{}
			return nil
		}
		if err != nil {
			return err
		}
		if currentProject.Name != name {
			return ErrFederationReplicaBindingConflict
		}
		if enrollmentPending && currentProject.DeletedAt != nil {
			return errFederationBridgePendingProjectArchived
		}
		project = currentProject
		binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if binding.Role != db.FederationRoleSpoke || binding.HubURL != target.hubURL || binding.HubProjectID != target.hubProjectID {
			return ErrFederationReplicaBindingConflict
		}
		return federationBridgeDownstreamCheck(ctx, cfg.DB, project.ID)
	}
	key := federationReplicaTransitionKey(cfg.DB, name)
	ensureFederationReplicaMu.Lock()
	err := validate()
	if err == nil && !preflight && credentialFound && !credential.LeavePending {
		replacement := credential
		replacement.LeavePending = true
		err = replacer.ReplaceFederationCredential(ctx, config.FederationCredentialReplacement{ProjectUID: target.projectUID, Expected: credential, Replacement: replacement})
		if err == nil {
			credential = replacement
		}
	}
	if err == nil && !preflight {
		federationReplicaTransitions.markLeavePending(key)
	}
	ensureFederationReplicaMu.Unlock()
	if err != nil {
		return nil, federationBridgeLocalDisconnectAPIError(err)
	}
	result := &api.DisconnectFederationBridgeResponse{Body: api.FederationBridgeDisconnectResult{ProjectName: name, ProjectUID: target.projectUID, Status: "ready", LocalOnly: true, Warning: federationBridgeLocalOnlyWarning}}
	if preflight {
		return result, nil
	}
	if err := waitFederationReplicaOperations(ctx, key); err != nil {
		return nil, federationBridgeLocalDisconnectAPIError(err)
	}
	if project.ID != 0 {
		finish, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), project.ID), cfg.DB, project.ID)
		if err != nil {
			return nil, federationBridgeLocalDisconnectAPIError(err)
		}
		defer finish()
	}
	ensureFederationReplicaMu.Lock()
	err = validate()
	if err == nil && project.ID != 0 {
		_, err = detacher.DetachRelayReplicaLocally(ctx, project.ID)
	}
	if err == nil && credentialFound {
		err = remover.DeleteFederationCredentialIfUnchanged(ctx, target.projectUID, credential)
	}
	if err == nil {
		federationReplicaTransitions.markLeft(key)
	}
	ensureFederationReplicaMu.Unlock()
	if err != nil {
		return nil, federationBridgeLocalDisconnectAPIError(err)
	}
	if cfg.FederationWake != nil {
		cfg.FederationWake()
	}
	result.Body.Status = "disconnected"
	return result, nil
}

// federationBridgeDownstreamCheck refuses while an active relay enrollment
// still receives this project through the bridge.
func federationBridgeDownstreamCheck(ctx context.Context, store db.Storage, projectID int64) error {
	enrollments, err := store.ListFederationEnrollments(ctx)
	if err != nil {
		return err
	}
	for _, enrollment := range enrollments {
		if enrollment.ProjectID != nil && *enrollment.ProjectID == projectID && enrollment.RelayProtocolVersion != 0 && enrollment.RevokedAt == nil {
			return db.ErrFederationResetBlockedByDownstream
		}
	}
	return nil
}

func federationBridgeLocalDisconnectAPIError(err error) error {
	if errors.Is(err, db.ErrFederationResetBlockedByDownstream) {
		return api.NewError(http.StatusConflict, "federation_downstream_enrollments", "downstream relay enrollments still depend on this bridge", "list them with `kata federation enrollments list`, revoke each with `kata federation revoke <id>`, then retry", nil)
	}
	return federationBridgeDisconnectAPIError(err)
}
