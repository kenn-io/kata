package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationpublication"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

const adoptionRetryHint = "retry the identical `kata federation bridge connect --adopt-existing` with the same project, hub and account"

func adoptionConflict(message string) error {
	return api.NewError(409, "federation_adoption_conflict", message, adoptionRetryHint, nil)
}

func adoptionPublicationRejected(projectName string) error {
	return api.NewError(409, "federation_adoption_rejected", "publication was definitely rejected; disconnect this unused adoption before starting again", "kata federation bridge disconnect --project "+projectName, nil)
}

func adoptionLeavePreparedError() error {
	return api.NewError(http.StatusConflict, "federation_adoption_conflict",
		"a federation leave is already prepared for this project",
		"finish or resume `kata federation leave` before starting relay adoption", nil)
}

func rejectAdoptionDuringPreparedFederationLeave(ctx context.Context, cfg ServerConfig, projectUID string) error {
	credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, projectUID)
	if err != nil {
		return credentialIOError("read federation credential before adoption admission")
	}
	if found && credential.LeavePending {
		return adoptionLeavePreparedError()
	}
	return nil
}

func adoptionRecoveryError() error {
	return api.NewError(409, "federation_adoption_recovery_required", "the frozen adoption envelope is missing or invalid; source writes remain paused", "restore the original private adoption artifact from backup, then "+adoptionRetryHint, nil)
}

type adoptionHub struct {
	catalog         config.CatalogDaemonConfig
	origin          string
	account         string
	instance        string
	signingRequired bool
	bridge          *generated.Client
	publish         func(context.Context, config.CatalogDaemonConfig, []byte, bool) (api.FederationPublicationBody, error)
}

func resolveAdoptionHub(ctx context.Context, cfg ServerConfig, in *api.ConnectFederationBridgeRequest) (adoptionHub, error) {
	var out adoptionHub
	catalog, err := federationRebindCatalogByName(cfg.FederationCatalog, in.Body.HubCatalog)
	if err != nil {
		return out, err
	}
	origin, err := httpurl.CanonicalHTTPOrigin(catalog.URL)
	base, baseErr := httpurl.CanonicalHTTPBaseURL(catalog.URL)
	if err != nil || baseErr != nil || base != origin || catalog.Local {
		return out, api.NewError(400, "validation", "hub_catalog must select a remote root HTTP(S) origin", "", nil)
	}
	token := strings.TrimSpace(catalog.Token)
	if catalog.TokenEnv != "" {
		token = strings.TrimSpace(os.Getenv(catalog.TokenEnv))
	}
	if token == "" {
		return out, api.NewError(400, "hub_credential_required", "selected hub has no user credential", "", nil)
	}
	transport := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := (config.BearerPolicy{AllowInsecurePlaintext: catalog.AllowInsecure}).ConfigureClient(transport, origin, token); err != nil {
		return out, api.NewError(400, "validation", "hub credential target violates transport policy", "", nil)
	}
	bridge, err := newFederationBridgeAPIClient(transport, origin)
	if err != nil {
		return out, internalAPIError(err)
	}
	response, callErr := bridge.InstanceWithResponse(ctx)
	var raw []byte
	if response != nil {
		raw = response.Body
	}
	var instance api.InstanceResponse
	if err := decodeFederationBridgeResponse(raw, callErr, &instance.Body); err != nil {
		return out, err
	}
	peer := instance.Body
	if peer.RelayPublicationVersion != 1 {
		return out, api.NewError(409, "federation_publication_unsupported", "hub does not support relay publication", "upgrade the selected hub before publishing", nil)
	}
	if !uid.Valid(peer.InstanceUID) || peer.InstanceUID == cfg.DB.InstanceUID() || (catalog.InstanceUID != "" && catalog.InstanceUID != peer.InstanceUID) {
		return out, adoptionConflict("selected hub identity does not match the catalog")
	}
	if peer.RelayProtocolVersion != db.RelayProtocolVersion || peer.ProvenanceProtocolVersion != 1 || peer.EmbeddingArtifactProtocolVersion != 1 {
		return out, api.NewError(409, "unsupported_relay_protocol", "hub lacks required relay protocols", "upgrade the selected hub", nil)
	}
	if peer.Auth.Kind != string(PrincipalDBToken) || peer.Auth.Scope != nil || db.ValidateTokenActor(peer.Auth.Actor) != nil {
		return out, api.NewError(403, "relay_user_credential_required", "selected hub requires an ordinary unscoped account credential", "", nil)
	}
	if !peer.WebUICapabilities.Writable {
		return out, api.NewError(409, "hub_readonly", "selected hub account is read-only", "select a writable hub account", nil)
	}
	catalog.URL = origin
	catalog.Token = token
	catalog.TokenEnv = ""
	if cfg.FederationAdoptionPublish == nil || cfg.FederationAdoptionSync == nil {
		return out, api.NewError(503, "federation_adoption_unavailable", "host has not configured relay adoption services", "", nil)
	}
	return adoptionHub{catalog: catalog, origin: origin, instance: peer.InstanceUID, account: peer.Auth.Actor, signingRequired: peer.FederationSigningRequired, bridge: bridge, publish: cfg.FederationAdoptionPublish}, nil
}

func adoptionHubError(err error) error {
	if e, ok := errors.AsType[*api.APIError](err); ok {
		// Hub authorization errors are upstream failures, never local logout.
		if e.Status == 401 || e.Status == 403 {
			return api.NewError(502, "hub_credential_rejected", "selected hub refused publication", adoptionRetryHint, nil)
		}
		return api.NewError(e.Status, e.Code, e.Message, adoptionRetryHint, nil)
	}
	return api.NewError(503, "hub_unavailable", "selected hub request did not complete", adoptionRetryHint, nil)
}

// ResumeFederationAdoption is the initial and exact-retry workflow. Only the
// publication section holds the exclusive transport gate; replica setup and
// immediate sync acquire their own gates and must run after it is released.
func ResumeFederationAdoption(ctx context.Context, cfg ServerConfig, in *api.ConnectFederationBridgeRequest) (api.FederationBridgeBody, error) {
	var body api.FederationBridgeBody
	if db.ValidateTokenActor(strings.TrimSpace(in.Body.Actor)) != nil || config.ValidateProjectName(in.Body.ProjectName) != nil || in.Body.HubProject != in.Body.ProjectName {
		return body, api.NewError(400, "validation", "adoption requires a valid local account and matching source and hub project names", "the published identity can be renamed on the hub after adoption", nil)
	}
	native, ok := cfg.DB.(db.FederationAdoptionStore)
	if !ok {
		return body, api.NewError(503, "federation_adoption_unavailable", "source store does not support adoption", "", nil)
	}
	project, err := cfg.DB.ProjectByName(ctx, in.Body.ProjectName)
	if err != nil {
		return body, internalAPIError(err)
	}
	hub, err := resolveAdoptionHub(ctx, cfg, in)
	if err != nil {
		return body, err
	}
	if _, err := federationBridgeSigningSource(ctx, cfg, in, hub.origin, project.UID, hub.signingRequired); err != nil {
		return body, err
	}
	body = api.FederationBridgeBody{HubCatalog: hub.catalog.Name, HubURL: hub.origin, HubInstanceUID: hub.instance, HubProjectUID: project.UID, ProjectID: project.ID, ProjectName: project.Name, LocalAccount: strings.TrimSpace(in.Body.Actor), UpstreamAccount: hub.account, Direction: "bidirectional", Status: "ready", Warning: "Source writes will pause until the initial relay synchronization is fully drained."}
	op, err := native.FederationAdoption(ctx, project.ID)
	found := err == nil
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return body, internalAPIError(err)
	}
	localActor := strings.TrimSpace(in.Body.Actor)
	if found && (op.ProjectUID != project.UID || op.SourceInstanceUID != cfg.DB.InstanceUID() || op.TargetInstanceUID != hub.instance || op.TargetOrigin != hub.origin || op.CatalogName != hub.catalog.Name || op.Account != hub.account || (op.LocalAccount != "" && op.LocalAccount != localActor) || op.ProjectName != project.Name) {
		return body, adoptionConflict("saved adoption selects a different account or destination")
	}
	if found && op.Phase == "rejected" {
		return body, adoptionPublicationRejected(project.Name)
	}
	if !found {
		if err := rejectAdoptionDuringPreparedFederationLeave(ctx, cfg, project.UID); err != nil {
			return body, err
		}
		existing, lookupErr := cfg.DB.FederationBindingByProject(ctx, project.ID)
		if lookupErr == nil {
			completed := db.FederationAdoptionOperation{SourceInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, TargetInstanceUID: hub.instance, TargetOrigin: hub.origin, CatalogName: hub.catalog.Name, Account: hub.account, ProjectName: project.Name, ProjectID: project.ID, HubProjectID: existing.HubProjectID}
			_, credential, err := activeAdoptionHop(ctx, cfg, completed, in)
			if err != nil {
				return body, err
			}
			if !in.Body.Preflight {
				if err := reauthorizeAdoptionParent(ctx, cfg, in, hub, completed, existing, credential); err != nil {
					return body, err
				}
			}
			body.HubProjectID = existing.HubProjectID
			body.Status = "connected"
			body.Phase = "complete"
			body.Warning = ""
			if in.Body.Preflight {
				body.Status = "ready"
			}
			return body, nil
		}
		if !errors.Is(lookupErr, db.ErrNotFound) {
			return body, internalAPIError(lookupErr)
		}
		if err := requireAdoptionActorProjectAccess(ctx, cfg.DB, localActor, project.UID); err != nil {
			return body, err
		}
		operationUID, err := uid.New()
		if err != nil {
			return body, internalAPIError(err)
		}
		op = db.FederationAdoptionOperation{OperationUID: operationUID, SourceInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, TargetInstanceUID: hub.instance, TargetOrigin: hub.origin, CatalogName: hub.catalog.Name, Account: hub.account, LocalAccount: localActor, ProjectName: project.Name, ProjectID: project.ID, Phase: "preparing"}
	}
	if in.Body.Preflight {
		if found {
			body.Status = "pending"
			body.OperationUID = op.OperationUID
			body.Phase = op.Phase
			body.SourcePaused = true
			body.HubProjectID = op.HubProjectID
			return body, nil
		}
		document, err := captureAdoption(ctx, cfg.DB, op)
		if err != nil {
			return body, err
		}
		raw, err := federationpublication.Encode(document)
		if err != nil {
			return body, adoptionSourceError(err)
		}
		preview, err := hub.publish(ctx, hub.catalog, raw, true)
		if err != nil {
			return body, adoptionHubError(err)
		}
		if preview.Account != hub.account {
			return body, adoptionConflict("hub publication account changed during preflight")
		}
		body.Visibility = preview.Visibility
		body.Counts = &preview.Counts
		return body, nil
	}
	home, err := config.KataHome()
	if err != nil {
		return body, internalAPIError(err)
	}
	finish, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), project.ID), cfg.DB, project.ID)
	if err != nil {
		return body, internalAPIError(err)
	}
	defer func() {
		if finish != nil {
			finish()
		}
	}()
	// Re-read after draining: an earlier exact request may have advanced it.
	current, readErr := native.FederationAdoption(ctx, project.ID)
	if readErr == nil {
		if current.OperationUID != op.OperationUID {
			return body, adoptionConflict("another adoption operation began during admission")
		}
		if current.Phase == "rejected" {
			return body, adoptionPublicationRejected(project.Name)
		}
		op = current
	} else if errors.Is(readErr, db.ErrNotFound) && !found {
		ensureFederationReplicaMu.Lock()
		if err := rejectAdoptionDuringPreparedFederationLeave(ctx, cfg, project.UID); err != nil {
			ensureFederationReplicaMu.Unlock()
			return body, err
		}
		beginCtx := db.WithAdditionalTransactionFence(ctx, func(fenceCtx context.Context, tx db.Transaction) error {
			err := cfg.DB.ProjectAccessTransactionFence(localActor, []string{project.UID})(fenceCtx, tx)
			if errors.Is(err, db.ErrNotFound) {
				return projectAccessDenied()
			}
			return err
		})
		op, err = native.BeginFederationAdoption(beginCtx, op)
		ensureFederationReplicaMu.Unlock()
		if err != nil {
			if apiErr, ok := errors.AsType[*api.APIError](err); ok {
				return body, apiErr
			}
			return body, adoptionSourceError(err)
		}
	} else {
		return body, adoptionConflict("saved adoption changed during admission")
	}
	if op.Phase == "preparing" {
		document, err := captureAdoption(ctx, cfg.DB, op)
		if err != nil {
			return body, err
		}
		raw, err := federationpublication.Encode(document)
		if err != nil {
			return body, adoptionSourceError(err)
		}
		path, err := config.WriteFederationAdoptionEnvelope(home, op.OperationUID, raw)
		if err != nil {
			return body, adoptionRecoveryError()
		}
		next := op
		next.EnvelopePath = path
		next.Digest = document.Digest
		next.Phase = "frozen"
		if err := native.AdvanceFederationAdoption(ctx, op, next); err != nil {
			return body, internalAPIError(err)
		}
		op = next
	}
	raw, _, err := readAdoptionEnvelope(home, op)
	if err != nil && op.Phase == "frozen" && errors.Is(err, os.ErrNotExist) {
		rebuilt, captureErr := captureAdoption(ctx, cfg.DB, op)
		if captureErr != nil {
			return body, captureErr
		}
		if rebuilt.Digest != op.Digest {
			return body, adoptionRecoveryError()
		}
		frozen, encodeErr := federationpublication.Encode(rebuilt)
		if encodeErr != nil {
			return body, adoptionRecoveryError()
		}
		if _, writeErr := config.WriteFederationAdoptionEnvelope(home, op.OperationUID, frozen); writeErr != nil {
			return body, adoptionRecoveryError()
		}
		raw, _, err = readAdoptionEnvelope(home, op)
	}
	if err != nil {
		return body, adoptionRecoveryError()
	}
	if op.Phase == "attached" {
		_, document, err := readAdoptionEnvelope(home, op)
		if err != nil {
			return body, adoptionRecoveryError()
		}
		counts, err := db.ValidateFederationPublication(document)
		if err != nil {
			return body, adoptionRecoveryError()
		}
		body.Counts = &api.FederationPublicationCounts{Issues: counts.Issues, Comments: counts.Comments, Events: counts.Events}
		body.HubProjectID = op.HubProjectID
		body.OperationUID = op.OperationUID
		body.Status = "pending"
		body.SourcePaused = true
		finish()
		finish = nil
		binding, credential, err := activeAdoptionHop(ctx, cfg, op, in)
		if err != nil {
			return body, err
		}
		if binding.RelayConfig.UpstreamRevoked {
			if err := reauthorizeAdoptionParent(ctx, cfg, in, hub, op, binding, credential); err != nil {
				return body, err
			}
		}
		return syncAdoption(ctx, cfg, body, binding, credential)
	}
	switch op.Phase {
	case "frozen", "transmitting", "published":
	default:
		return body, adoptionConflict("saved adoption phase cannot be transmitted")
	}
	firstAttempt := op.Phase == "frozen"
	if firstAttempt {
		next := op
		next.Phase = "transmitting"
		if err := native.AdvanceFederationAdoption(ctx, op, next); err != nil {
			return body, internalAPIError(err)
		}
		op = next
	}
	// Even known publication retries use the exact immutable document, never
	// resolve/create by a name which a hub user may have since renamed.
	published, err := hub.publish(ctx, hub.catalog, raw, false)
	if err != nil {
		if e, ok := errors.AsType[*api.APIError](err); ok && firstAttempt && definitePublicationNoncommit(e) {
			next := op
			next.Phase = "rejected"
			if advanceErr := native.AdvanceFederationAdoption(ctx, op, next); advanceErr != nil {
				return body, internalAPIError(advanceErr)
			}
		}
		return body, adoptionHubError(err)
	}
	if published.Account != op.Account || published.ProjectUID != op.ProjectUID || (op.HubProjectID != 0 && op.HubProjectID != published.ProjectID) {
		return body, adoptionConflict("hub publication returned a different identity or account")
	}
	if op.Phase == "transmitting" {
		next := op
		next.Phase = "published"
		next.HubProjectID = published.ProjectID
		if err := native.AdvanceFederationAdoption(ctx, op, next); err != nil {
			return body, internalAPIError(err)
		}
		op = next
	}
	body.HubProjectID = published.ProjectID
	body.Visibility = published.Visibility
	body.Counts = &published.Counts
	body.OperationUID = op.OperationUID
	body.SourcePaused = true
	body.Status = "pending"
	finish()
	finish = nil
	binding, credential, err := attachAdoption(ctx, cfg, in, hub, op, published.ProjectName)
	if err != nil {
		return body, err
	}
	if err := markAdoptionAttached(ctx, cfg, binding); err != nil {
		return body, internalAPIError(err)
	}
	return syncAdoption(ctx, cfg, body, binding, credential)
}

func definitePublicationNoncommit(err *api.APIError) bool {
	// Authorization/concealed lookup failures can occur after commit while
	// constructing the response. Only publication's precommit contract codes
	// prove a first request unused; later refusals never prove noncommit.
	switch err.Code {
	case "federation_publication_invalid":
		return err.Status == 400
	case "federation_publication_too_large":
		return err.Status == 413
	case "federation_publication_conflict":
		return err.Status == 409
	default:
		return false
	}
}

func activeAdoptionHop(ctx context.Context, cfg ServerConfig, op db.FederationAdoptionOperation, in *api.ConnectFederationBridgeRequest) (db.FederationBinding, config.FederationCredential, error) {
	binding, err := cfg.DB.FederationBindingByProject(ctx, op.ProjectID)
	if err != nil {
		return binding, config.FederationCredential{}, adoptionConflict("saved adoption binding is unavailable")
	}
	candidate, found, err := cfg.federationCredentialStore().FederationCredential(ctx, op.ProjectUID)
	if err != nil {
		return binding, candidate, credentialIOError("read activated adoption credential")
	}
	if !found || binding.RelayConfig == nil || binding.RelayConfig.LocalActor != strings.TrimSpace(in.Body.Actor) || binding.RelayConfig.ServeDownstream != in.Body.ServeDownstream || (op.BindingUID != "" && binding.RelayConfig.BindingUID != op.BindingUID) || binding.RelayConfig.ResetEpoch < op.ResetEpoch {
		return binding, candidate, adoptionConflict("activated relay differs from the selected adoption")
	}
	current := op
	current.BindingUID = binding.RelayConfig.BindingUID
	current.ResetEpoch = binding.RelayConfig.ResetEpoch
	if !db.FederationAdoptionBindingMatches(current, binding) {
		return binding, candidate, adoptionConflict("activated relay selects a different account or destination")
	}
	if err := adoptionCredentialProof(cfg, candidate)(ctx, nil, current, binding); err != nil {
		return binding, candidate, adoptionConflict("activated relay credential differs from the selected adoption")
	}
	return binding, candidate, nil
}

// Replacing a revoked account token must reparent the existing narrow grant.
// Keep the active transport identity and local epoch: only authenticated sync
// may recover revocation/reset state and release the adoption pause.
func reauthorizeAdoptionParent(ctx context.Context, cfg ServerConfig, in *api.ConnectFederationBridgeRequest, hub adoptionHub, op db.FederationAdoptionOperation, binding db.FederationBinding, credential config.FederationCredential) error {
	finishGate, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), op.ProjectID), cfg.DB, op.ProjectID)
	if err != nil {
		return err
	}
	defer finishGate()
	current, err := cfg.DB.FederationBindingByProject(ctx, op.ProjectID)
	if err != nil || !reflect.DeepEqual(current, binding) {
		return adoptionConflict("activated relay changed before account credential recovery")
	}
	finish, err := beginFederationBridgeEnrollment(ctx, cfg.DB, cfg.federationCredentialStore(), op.ProjectName, op.ProjectUID, credential)
	if err != nil {
		return err
	}
	defer finish()
	rebind := true
	response, callErr := hub.bridge.CreateFederationEnrollmentWithResponse(ctx, &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{
		ProjectID: op.HubProjectID, SpokeInstanceUID: op.SourceInstanceUID, Token: &credential.Token, Capabilities: "claim,pull,push",
		Relay: &generated.RelayEnrollmentOptions{ProtocolVersion: int64(db.RelayProtocolVersion), ServeDownstream: &in.Body.ServeDownstream, RebindParent: &rebind},
	}})
	var raw []byte
	if response != nil {
		raw = response.Body
	}
	var grant api.FederationEnrollmentOut
	if err := decodeFederationBridgeResponse(raw, callErr, &grant); err != nil {
		return err
	}
	if grant.Relay == nil || grant.ProjectID == nil || *grant.ProjectID != op.HubProjectID || grant.Actor != op.Account || grant.SpokeInstanceUID != op.SourceInstanceUID || grant.Token != credential.Token || grant.Capabilities != credential.Capabilities || grant.Relay.UpstreamInstanceUID != op.TargetInstanceUID || grant.Relay.Root.ProjectUID != op.ProjectUID || grant.Relay.Root.AuthorityUID != binding.RelayConfig.AuthorityUID || grant.Relay.BindingUID != binding.RelayConfig.BindingUID || grant.Relay.ResetEpoch < binding.RelayConfig.ResetEpoch {
		return adoptionConflict("account credential recovery returned a different relay identity")
	}
	return nil
}

func syncAdoption(ctx context.Context, cfg ServerConfig, body api.FederationBridgeBody, binding db.FederationBinding, credential config.FederationCredential) (api.FederationBridgeBody, error) {
	// One bounded turn only. The runner/exact retry completes any remaining work.
	syncErr := cfg.FederationAdoptionSync(ctx, binding, credential, cfg.Publish().Events, func(c context.Context, b db.FederationBinding) error {
		return FinalizeFederationAdoptionAfterSync(c, cfg, b)
	})
	remaining, err := cfg.DB.(db.FederationAdoptionStore).FederationAdoption(ctx, binding.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		body.Status = "connected"
		body.Phase = "complete"
		body.SourcePaused = false
		body.Warning = ""
		return body, nil
	}
	if err != nil {
		return body, internalAPIError(err)
	}
	body.Phase = remaining.Phase
	body.Warning = "Source writes remain paused; " + adoptionRetryHint
	if syncErr != nil && cfg.FederationWake != nil {
		cfg.FederationWake()
	}
	return body, nil
}

func requireAdoptionActorProjectAccess(ctx context.Context, store db.Storage, actor, projectUID string) error {
	accessible, err := store.AccessibleProjectUIDs(ctx, actor)
	if err != nil {
		return internalAPIError(err)
	}
	for _, accessibleUID := range accessible {
		if accessibleUID == projectUID {
			return nil
		}
	}
	return projectAccessDenied()
}

func captureAdoption(ctx context.Context, store db.Storage, op db.FederationAdoptionOperation) (db.FederationPublication, error) {
	document, err := federationpublication.Capture(ctx, store, op.ProjectID, db.FederationPublication{OperationUID: op.OperationUID, SourceInstanceUID: op.SourceInstanceUID, ProjectUID: op.ProjectUID, TargetInstanceUID: op.TargetInstanceUID, ProjectName: op.ProjectName})
	if err != nil {
		return document, adoptionSourceError(err)
	}
	return document, nil
}

func adoptionSourceError(err error) error {
	if errors.Is(err, db.ErrFederationPublicationTooLarge) {
		return api.NewError(413, "federation_publication_too_large", "source exceeds publication capacity", "reduce the supported source before retrying", nil)
	}
	return api.NewError(409, "federation_adoption_ineligible", "source is not an eligible standalone project with self-contained replayable history", "choose a source without project.author_rewritten history; resolve federation, claims, integrations, automation, purge/reset boundaries or external historical references before adopting", nil)
}

func readAdoptionEnvelope(home string, op db.FederationAdoptionOperation) ([]byte, db.FederationPublication, error) {
	raw, err := config.ReadFederationAdoptionEnvelope(home, op.OperationUID, op.Digest)
	if err != nil {
		return nil, db.FederationPublication{}, err
	}
	document, err := federationpublication.Decode(raw)
	if err != nil {
		return nil, document, err
	}
	if document.OperationUID != op.OperationUID || document.SourceInstanceUID != op.SourceInstanceUID || document.ProjectUID != op.ProjectUID || document.TargetInstanceUID != op.TargetInstanceUID || document.ProjectName != op.ProjectName || document.Digest != op.Digest {
		return nil, document, db.ErrFederationAdoptionConflict
	}
	return raw, document, nil
}

func attachAdoption(ctx context.Context, cfg ServerConfig, in *api.ConnectFederationBridgeRequest, hub adoptionHub, op db.FederationAdoptionOperation, hubName string) (db.FederationBinding, config.FederationCredential, error) {
	var empty db.FederationBinding
	credentials := cfg.federationCredentialStore()
	candidate, found, err := credentials.FederationCredential(ctx, op.ProjectUID)
	if err != nil {
		return empty, candidate, credentialIOError("read adoption candidate")
	}
	signing, err := federationBridgeSigningSource(ctx, cfg, in, hub.origin, op.ProjectUID, hub.signingRequired)
	if err != nil {
		return empty, candidate, err
	}
	if found {
		if !candidate.ManagedByConfig || candidate.Provider != nil || candidate.LeavePending || candidate.HubURL != op.TargetOrigin || candidate.HubProjectID != op.HubProjectID || candidate.Actor != op.Account || candidate.HubCatalog != op.CatalogName || candidate.SpokeProjectName != op.ProjectName || candidate.RequestedActor != strings.TrimSpace(in.Body.Actor) || candidate.Token == "" || candidate.Capabilities != "claim,pull,push" {
			return empty, candidate, adoptionConflict("saved adoption credential selects a different bridge")
		}
	} else {
		token, err := newPlaintextToken()
		if err != nil {
			return empty, candidate, internalAPIError(err)
		}
		candidate = config.FederationCredential{HubURL: op.TargetOrigin, HubProjectID: op.HubProjectID, Token: token, Actor: op.Account, Capabilities: "claim,pull,push", ManagedByConfig: true, HubCatalog: op.CatalogName, HubProjectName: hubName, SpokeProjectName: op.ProjectName, RequestedActor: strings.TrimSpace(in.Body.Actor), AllowInsecure: hub.catalog.AllowInsecure, RelayEnrollmentPending: true, Signing: signing}
		if err := ReserveFederationReplicaCredential(ctx, cfg.DB, credentials, ReserveFederationReplicaCredentialParams{HubProjectUID: op.ProjectUID, ProjectName: op.ProjectName, Credential: candidate}); err != nil {
			return empty, candidate, federationReplicaAPIError(err)
		}
	}
	enabled, callErr := hub.bridge.EnableProjectFederationWithResponse(ctx, &generated.EnableProjectFederationRequestOptions{PathParams: &generated.EnableProjectFederationPath{ProjectID: op.HubProjectID}, Body: &generated.EnableProjectFederationBody{Actor: &hub.account}})
	var raw []byte
	if enabled != nil {
		raw = enabled.Body
	}
	var metadata api.ProjectFederationBody
	if err := decodeFederationBridgeResponse(raw, callErr, &metadata); err != nil {
		return empty, candidate, err
	}
	version := int64(db.RelayProtocolVersion)
	response, callErr := hub.bridge.GetProjectFederationWithResponse(ctx, &generated.GetProjectFederationRequestOptions{PathParams: &generated.GetProjectFederationPath{ProjectID: op.HubProjectID}, Query: &generated.GetProjectFederationQuery{RelayProtocolVersion: &version}})
	raw = nil
	if response != nil {
		raw = response.Body
	}
	if err := decodeFederationBridgeResponse(raw, callErr, &metadata); err != nil {
		return empty, candidate, err
	}
	if metadata.ProjectID != op.HubProjectID || metadata.ProjectUID != op.ProjectUID || metadata.ReplayHorizonEventID <= 0 {
		return empty, candidate, adoptionConflict("published project metadata does not match the frozen identity")
	}
	finish, err := beginFederationBridgeEnrollment(ctx, cfg.DB, credentials, op.ProjectName, op.ProjectUID, candidate)
	if err != nil {
		return empty, candidate, err
	}
	rebind := true
	granted, callErr := hub.bridge.CreateFederationEnrollmentWithResponse(ctx, &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{ProjectID: op.HubProjectID, SpokeInstanceUID: cfg.DB.InstanceUID(), Token: &candidate.Token, Capabilities: "claim,pull,push", Relay: &generated.RelayEnrollmentOptions{ProtocolVersion: int64(db.RelayProtocolVersion), ServeDownstream: &in.Body.ServeDownstream, RebindParent: &rebind}}})
	finish()
	raw = nil
	if granted != nil {
		raw = granted.Body
	}
	var grant api.FederationEnrollmentOut
	if err := decodeFederationBridgeResponse(raw, callErr, &grant); err != nil {
		return empty, candidate, err
	}
	if grant.Relay == nil || grant.ProjectID == nil || *grant.ProjectID != op.HubProjectID || grant.Relay.Root.ProjectUID != op.ProjectUID || grant.Relay.UpstreamInstanceUID != op.TargetInstanceUID || grant.Actor != op.Account || grant.Token != candidate.Token || grant.SpokeInstanceUID != op.SourceInstanceUID || grant.Capabilities != "claim,pull,push" {
		return empty, candidate, adoptionConflict("hub returned an inconsistent adoption grant")
	}
	next := op
	next.BindingUID = grant.Relay.BindingUID
	next.ResetEpoch = grant.Relay.ResetEpoch
	if next != op {
		if err := cfg.DB.(db.FederationAdoptionStore).AdvanceFederationAdoption(ctx, op, next); err != nil {
			return empty, candidate, internalAPIError(err)
		}
		op = next
	}
	active := candidate
	active.RelayEnrollmentPending = false
	result, err := EnsureFederationReplica(db.WithFederationAdoptionAttachment(ctx, op), cfg.DB, credentials, cfg.FederationWake, EnsureFederationReplicaParams{HubURL: op.TargetOrigin, HubProjectID: op.HubProjectID, HubProjectUID: op.ProjectUID, ProjectName: op.ProjectName, ReplayHorizonEventID: metadata.ReplayHorizonEventID, Credential: active, ExpectedCredential: &candidate, ManagedReservation: &FederationReplicaManagedReservation{ProjectUID: op.ProjectUID, Expected: candidate}, PushEnabled: true, Relay: grant.Relay, RelayLocalActor: active.RequestedActor, RelayServeDownstream: in.Body.ServeDownstream, ProjectEventSink: func(event db.Event) { deliverProjectMutation(cfg, &event) }})
	if err != nil {
		return result.Binding, candidate, federationReplicaAPIError(err)
	}
	return result.Binding, active, nil
}

// adoptionCredentialProof is called inside the native transaction while the
// caller retains the project transport gate and replica transition mutex.
func adoptionCredentialProof(cfg ServerConfig, expected config.FederationCredential) db.FederationAdoptionCredentialValidator {
	return func(ctx context.Context, _ db.Transaction, op db.FederationAdoptionOperation, binding db.FederationBinding) error {
		current, found, err := cfg.federationCredentialStore().FederationCredential(ctx, op.ProjectUID)
		if err != nil {
			return db.ErrTransactionFinalizationFailed
		}
		if !found || !current.Equal(expected) || !current.ManagedByConfig || current.RelayEnrollmentPending || current.LeavePending || current.Provider != nil || current.Token == "" || current.Actor != op.Account || current.HubURL != op.TargetOrigin || current.HubProjectID != op.HubProjectID || current.HubCatalog != op.CatalogName || current.SpokeProjectName != op.ProjectName || binding.RelayConfig == nil || current.RequestedActor != binding.RelayConfig.LocalActor || current.Capabilities != "claim,pull,push" {
			return db.ErrFederationAdoptionConflict
		}
		return nil
	}
}

func advanceAdoptionAttachedLocked(ctx context.Context, cfg ServerConfig, binding db.FederationBinding) (db.FederationAdoptionOperation, context.Context, error) {
	native := cfg.DB.(db.FederationAdoptionStore)
	op, err := native.FederationAdoption(ctx, binding.ProjectID)
	if err != nil {
		return op, ctx, err
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, binding.ProjectID)
	if err != nil {
		return op, ctx, err
	}
	if !reflect.DeepEqual(current, binding) || binding.RelayConfig == nil {
		return op, ctx, db.ErrFederationAdoptionConflict
	}
	candidate, found, err := cfg.federationCredentialStore().FederationCredential(ctx, op.ProjectUID)
	if err != nil {
		return op, ctx, err
	}
	if !found {
		return op, ctx, db.ErrFederationAdoptionConflict
	}
	next := op
	next.Phase = "attached"
	next.BindingUID = binding.RelayConfig.BindingUID
	next.ResetEpoch = binding.RelayConfig.ResetEpoch
	if op.BindingUID != next.BindingUID || op.ResetEpoch > next.ResetEpoch || !db.FederationAdoptionBindingMatches(next, binding) {
		return op, ctx, db.ErrFederationAdoptionConflict
	}
	proof := adoptionCredentialProof(cfg, candidate)
	if err := proof(ctx, nil, next, binding); err != nil {
		return op, ctx, err
	}
	fenced := db.WithFederationAdoptionFinalization(ctx, next, binding, proof)
	if next != op {
		if err := native.AdvanceFederationAdoption(fenced, op, next); err != nil {
			return op, ctx, err
		}
	}
	return next, fenced, nil
}

func markAdoptionAttached(ctx context.Context, cfg ServerConfig, binding db.FederationBinding) error {
	finish, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), binding.ProjectID), cfg.DB, binding.ProjectID)
	if err != nil {
		return err
	}
	defer finish()
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	_, _, err = advanceAdoptionAttachedLocked(ctx, cfg, binding)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	return err
}

// FinalizeFederationAdoptionAfterSync runs only from the drained callback,
// which already holds the exact project's shared transport/rebind gate. Taking
// an exclusive gate here would deadlock that turn. Keep credential transition
// coordination until the native proof and fence release have committed.
func FinalizeFederationAdoptionAfterSync(ctx context.Context, cfg ServerConfig, binding db.FederationBinding) error {
	native, ok := cfg.DB.(db.FederationAdoptionStore)
	if !ok {
		return nil
	}
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	_, err := native.FederationAdoption(ctx, binding.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	op, fenced, err := advanceAdoptionAttachedLocked(ctx, cfg, binding)
	if err != nil {
		return err
	}
	released, err := native.FinalizeFederationAdoption(fenced, op, binding)
	if err != nil {
		return err
	}
	if released {
		home, err := config.KataHome()
		if err != nil {
			return err
		}
		return config.RemoveFederationAdoptionEnvelope(home, op.OperationUID)
	}
	return nil
}

// Cancellation shares publication's exclusive project gate, so it cannot
// mistake an in-flight transmitting request for an unused operation.
func disconnectFederationAdoption(ctx context.Context, cfg ServerConfig, in *api.DisconnectFederationBridgeRequest) (*api.DisconnectFederationBridgeResponse, bool, error) {
	native, ok := cfg.DB.(db.FederationAdoptionStore)
	if !ok {
		return nil, false, nil
	}
	project, err := cfg.DB.ProjectByNameIncludingArchived(ctx, in.ProjectName)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	_, err = native.FederationAdoption(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	finish, err := federationcoord.BeginRebind(ctx, federationcoord.Key(cfg.DB.InstanceUID(), project.ID), cfg.DB, project.ID)
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	defer finish()
	op, err := native.FederationAdoption(ctx, project.ID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	if op.Phase != "preparing" && op.Phase != "frozen" && op.Phase != "rejected" {
		return nil, true, api.NewError(409, "federation_adoption_resume_required", "publication may have committed; source writes remain paused", adoptionRetryHint+" to resolve publication and complete initial synchronization before disconnecting", nil)
	}
	result := &api.DisconnectFederationBridgeResponse{Body: api.FederationBridgeDisconnectResult{ProjectName: project.Name, ProjectUID: project.UID, Status: "ready"}}
	if in.Body.Preflight {
		return result, true, nil
	}
	if err := native.CancelUnusedFederationAdoption(ctx, op); err != nil {
		return nil, true, internalAPIError(err)
	}
	home, err := config.KataHome()
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	if err := config.RemoveFederationAdoptionEnvelope(home, op.OperationUID); err != nil {
		return nil, true, api.NewError(500, "federation_adoption_cleanup_failed", "adoption canceled and source write pause was released; private artifact cleanup failed", "remove the unreferenced private adoption artifact for operation "+op.OperationUID, nil)
	}
	result.Body.Status = "disconnected"
	return result, true, nil
}
