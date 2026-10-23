package daemon

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func registerRelayHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "disconnectRelayEnrollment", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/federation/relay:disconnect", Summary: "Revoke only the presented relay transport grant"}, func(ctx context.Context, in *api.DisconnectRelayRequest) (*api.DisconnectRelayResponse, error) {
		token := ""
		if after, ok := strings.CutPrefix(in.Authorization, authBearerPrefix); ok {
			token = after
		}
		revoker, ok := cfg.DB.(db.RelaySelfRevoker)
		if !ok {
			return nil, relayTransportError(db.ErrTransactionFinalizationFailed)
		}
		if err := revoker.RevokeOwnRelayEnrollment(ctx, token, in.ProjectID, in.Body.SpokeInstanceUID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, api.NewError(404, "not_found", "resource not found", "", nil)
			}
			return nil, relayTransportError(err)
		}
		out := &api.DisconnectRelayResponse{}
		out.Body.Revoked = true
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{OperationID: "getRelayReset", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/federation/relay/reset"}, func(ctx context.Context, in *api.RelayResetRequest) (*api.RelayResetResponse, error) {
		ctx, principal, err := authorizeRelayRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", "getRelayReset")
		if err != nil {
			return nil, err
		}
		store, ok := cfg.DB.(db.RelayResetStore)
		if !ok {
			return nil, relayTransportError(db.ErrTransactionFinalizationFailed)
		}
		var signer db.RootAttributionSigner
		if cfg.RootSigning != nil {
			signer = cfg.RootSigning.Signer()
		}
		checkpoint, err := store.CreateRelayReset(ctx, principal.RelayBindingUID, signer)
		if err != nil {
			return nil, relayTransportError(err)
		}
		return &api.RelayResetResponse{Body: checkpoint}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "offerRelayDeliveries", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/federation/relay"}, func(ctx context.Context, in *api.RelayOfferRequest) (*api.RelayOfferResponse, error) {
		ctx, principal, err := authorizeRelayRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", "offerRelayDeliveries")
		if err != nil {
			return nil, err
		}
		if len(in.ArtifactDigests) > 0 {
			if in.Stream != db.RelayStreamArtifact {
				return nil, relayTransportError(db.ErrFederationIngestValidation)
			}
			store, ok := cfg.DB.(db.RelayArtifactStorage)
			if !ok {
				return nil, relayTransportError(db.ErrTransactionFinalizationFailed)
			}
			artifacts, err := store.RelayArtifactPayloads(ctx, principal.RelayBindingUID, in.Epoch, in.ArtifactDigests)
			if err != nil {
				return nil, relayTransportError(err)
			}
			return &api.RelayOfferResponse{Body: db.RelayBatch{Stream: db.RelayStreamArtifact, Artifacts: artifacts}}, nil
		}
		ctx = db.WithRelayRequestedEpoch(ctx, in.Epoch)
		envelopes, err := cfg.DB.PendingRelayDeliveries(ctx, principal.RelayBindingUID, in.Stream, in.Limit)
		if err != nil {
			return nil, relayTransportError(err)
		}
		// Zero is a valid replay baseline. Durable per-hop acceptance rejects
		// skipped prefixes and deduplicates the retained envelopes after lost ACKs.
		return &api.RelayOfferResponse{Body: db.RelayBatch{Stream: in.Stream, Envelopes: envelopes}}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "acceptRelayDeliveries", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/federation/relay:accept", MaxBodyBytes: 128 << 20}, func(ctx context.Context, in *api.RelayAcceptRequest) (*api.RelayAcceptResponse, error) {
		ctx, principal, err := authorizeRelayRequest(ctx, cfg, in.Authorization, in.ProjectID, "push", "acceptRelayDeliveries")
		if err != nil {
			return nil, err
		}
		if cfg.RootSigning != nil {
			ctx = db.WithRootAttribution(ctx, cfg.RootSigning.Signer(), principal.Actor)
		}
		result, err := cfg.DB.AcceptRelayDeliveries(ctx, principal.RelayBindingUID, in.Body)
		if err != nil {
			return nil, relayTransportError(err)
		}
		if len(result.InsertedEvents) > 0 {
			cfg.Publish().Events(in.ProjectID, result.InsertedEvents)
		}
		// Work accepted from a downstream relay is owed upstream; the wake
		// forwarder ignores these remote-origin events, so wake here.
		if in.Body.Stream != db.RelayStreamArtifact && result.Through > 0 && cfg.FederationWake != nil {
			cfg.FederationWake()
		}
		if in.Body.Stream == db.RelayStreamArtifact && cfg.EmbeddingWake != nil && (len(in.Body.Artifacts) > 0 || result.Through > 0) {
			cfg.EmbeddingWake()
		}
		return &api.RelayAcceptResponse{Body: result}, nil
	})
	huma.Register(humaAPI, huma.Operation{OperationID: "ackRelayDeliveries", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/federation/relay:ack"}, func(ctx context.Context, in *api.RelayAckRequest) (*api.RelayAckResponse, error) {
		ctx, principal, err := authorizeRelayRequest(ctx, cfg, in.Authorization, in.ProjectID, "pull", "ackRelayDeliveries")
		if err != nil {
			return nil, err
		}
		if err := cfg.DB.AckRelayDeliveries(ctx, principal.RelayBindingUID, in.Body.Epoch, in.Body.Stream, in.Body.Through, in.Body.Digest); err != nil {
			return nil, relayTransportError(err)
		}
		out := &api.RelayAckResponse{}
		out.Body.Acknowledged = true
		return out, nil
	})
}

func authorizeRelayRequest(ctx context.Context, cfg ServerConfig, auth string, projectID int64, capability, operation string) (context.Context, federationPrincipal, error) {
	ctx, principal, err := authorizeFederationRequest(ctx, cfg, auth, projectID, capability, federationTransportOperation(operation))
	if err != nil {
		return ctx, principal, err
	}
	if principal.RelayProtocolVersion != db.RelayProtocolVersion || principal.RelayBindingUID == "" {
		return ctx, principal, federationCredentialDenied()
	}
	return ctx, principal, nil
}

func relayTransportError(err error) error {
	if apiErr := relayCrossProjectLinksAPIError(err); apiErr != nil {
		return apiErr
	}
	if apiErr := rootSigningKeyAPIError(err); apiErr != nil {
		return apiErr
	}
	if apiErr := relayProjectTooLargeAPIError(err); apiErr != nil {
		return apiErr
	}
	if errors.Is(err, db.ErrEmbeddingArtifactMiss) {
		return api.NewError(http.StatusConflict, "artifact_miss", "artifact is temporarily unavailable; retry after content synchronization", "", nil)
	}
	if errors.Is(err, db.ErrFederationResetBlockedByPendingPush) || errors.Is(err, db.ErrFederationResetBlockedByQuarantine) || errors.Is(err, db.ErrFederationResetBlockedByExternalRoot) {
		return api.NewError(http.StatusConflict, "relay_reset_blocked", "relay reset is blocked by retained work or bindings", "", nil)
	}
	if errors.Is(err, db.ErrNotFound) {
		return federationCredentialDenied()
	}
	if errors.Is(err, ErrHostAccessDenied) {
		return projectAccessDenied()
	}
	if errors.Is(err, db.ErrRemoteEventConflict) {
		return api.NewError(http.StatusConflict, "relay_conflict", "relay source identity conflicts with retained content", "", nil)
	}
	if errors.Is(err, db.ErrFederationIngestValidation) || errors.Is(err, db.ErrRemoteEventHashMismatch) {
		// Do not expose source, constraint or project details in protocol errors.
		return api.NewError(http.StatusBadRequest, "relay_invalid", "relay batch, epoch or acknowledgement is invalid", "", nil)
	}
	if errors.Is(err, db.ErrTransactionFinalizationFailed) || errors.Is(err, errHostFederationAccessUnavailable) {
		return api.NewError(http.StatusServiceUnavailable, "access_unavailable", "relay storage or authorization is unavailable", "", nil)
	}
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		if apiErr.Status == http.StatusForbidden || apiErr.Status == http.StatusNotFound {
			return projectAccessDenied()
		}
		if apiErr.Status == http.StatusServiceUnavailable {
			return api.NewError(http.StatusServiceUnavailable, "access_unavailable", "relay storage or authorization is unavailable", "", nil)
		}
	}
	return api.NewError(http.StatusInternalServerError, "internal", "relay storage is unavailable", "", nil)
}

// relayCrossProjectLinksAPIError intentionally omits endpoints: a project
// member may not be allowed to see the other project involved in a link.
func relayCrossProjectLinksAPIError(err error) *api.APIError {
	if !errors.Is(err, db.ErrFederationCrossProjectBoundary) {
		return nil
	}
	return newRelayCrossProjectLinksError("local")
}

// relayProjectLinksCheckError maps a read-only link check outside relay
// transport. A project archived since its lookup is not a credential failure.
func relayProjectLinksCheckError(err error) error {
	if apiErr := relayCrossProjectLinksAPIError(err); apiErr != nil {
		return apiErr
	}
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
	}
	return internalAPIError(err)
}

func newRelayCrossProjectLinksError(side string) *api.APIError {
	message := "this daemon's copy of the project has cross-project issue links"
	location := "locally"
	if side == "hub" {
		message = "the project on the hub has cross-project issue links"
		location = "on the hub"
	}
	return api.NewError(http.StatusConflict, "relay_cross_project_links",
		message, "remove cross-project related, blocks, and parent links "+location+" before retrying; ask an operator on that side to remove links to projects you cannot access", nil)
}

func createRelayEnrollment(ctx context.Context, cfg ServerConfig, in *api.CreateFederationEnrollmentRequest) (*api.CreateFederationEnrollmentResponse, error) {
	options := in.Body.Relay
	if options.ProtocolVersion != db.RelayProtocolVersion || in.Body.ProjectID == nil || *in.Body.ProjectID <= 0 || !uid.Valid(in.Body.SpokeInstanceUID) || in.Body.AllowAdoptionSnapshotAuthors {
		return nil, api.NewError(http.StatusBadRequest, "validation", "relay requires a supported protocol, one project and a valid peer", "", nil)
	}
	principal, ok := PrincipalFromContext(ctx)
	if !ok || principal.Kind != PrincipalDBToken || principal.TokenID <= 0 || principal.Scope != nil {
		return nil, api.NewError(http.StatusForbidden, "relay_user_credential_required", "relay enrollment requires an ordinary user credential", "", nil)
	}
	if in.Body.Actor != "" && in.Body.Actor != principal.Actor {
		return nil, api.NewError(http.StatusBadRequest, "validation", "relay actor must match the user credential", "", nil)
	}
	caps, err := db.CanonicalFederationCapabilities(in.Body.Capabilities)
	if err != nil || caps != "claim,pull,push" {
		return nil, api.NewError(http.StatusBadRequest, "validation", "relay requires claim,pull,push capabilities", "", nil)
	}
	ctx, err = authorizeHostProjectScope(ctx, []int64{*in.Body.ProjectID}, nil, false)
	if err != nil {
		return nil, err
	}
	if options.ServeDownstream {
		if err := requireRelayDownstreamAuthority(ctx, cfg.DB, *in.Body.ProjectID, principal.Actor); err != nil {
			return nil, err
		}
	}
	// Resolve the public pin and topology before issuing the durable secret.
	handshake, err := relayHandshake(ctx, cfg.DB, *in.Body.ProjectID, in.Body.SpokeInstanceUID, options.ServeDownstream)
	if err != nil {
		return nil, err
	}
	created, err := cfg.DB.CreateRelayEnrollment(ctx, db.CreateRelayEnrollmentParams{ProjectID: *in.Body.ProjectID, ParentTokenID: principal.TokenID, SpokeInstanceUID: in.Body.SpokeInstanceUID, ProtocolVersion: options.ProtocolVersion, Actor: principal.Actor, Token: in.Body.Token, ServeDownstream: options.ServeDownstream, RebindParent: options.RebindParent, RootKeyID: handshake.Root.KeyID})
	if errors.Is(err, db.ErrFederationEnrollmentTokenConflict) {
		return nil, api.NewError(http.StatusConflict, "federation_enrollment_token_conflict", "federation enrollment token conflicts with an existing enrollment", "", nil)
	}
	if errors.Is(err, db.ErrRelayRootChanged) {
		return nil, api.NewError(http.StatusConflict, "relay_root_changed", "the project's root signing key changed while the relay was enrolling", "retry the enrollment to receive the current root key", nil)
	}
	if apiErr := relayProjectTooLargeAPIError(err); apiErr != nil {
		return nil, apiErr
	}
	if errors.Is(err, db.ErrNotFound) {
		return nil, projectAccessDenied()
	}
	if err != nil {
		return nil, relayTransportError(err)
	}
	handshake.BindingUID = created.Enrollment.RelayBindingUID
	handshake.ResetEpoch = created.Enrollment.RelayResetEpoch
	handshake.ResetRequired, err = relayEnrollmentNeedsReset(ctx, cfg.DB, handshake.BindingUID)
	if err != nil {
		return nil, relayTransportError(err)
	}
	out := federationEnrollmentToOut(created.Enrollment, created.Token)
	out.Relay = handshake
	return &api.CreateFederationEnrollmentResponse{Body: out}, nil
}

// relayProjectTooLargeAPIError reports a project whose signed checkpoint no
// longer fits the relay checkpoint capacity. The size and capacity are the
// project's own, so the enrolling operator can act on them.
func relayProjectTooLargeAPIError(err error) error {
	tooLarge, ok := errors.AsType[*db.RelayProjectTooLargeError](err)
	if !ok {
		return nil
	}
	return api.NewError(http.StatusConflict, "relay_project_too_large",
		fmt.Sprintf("the project's relay checkpoint is %d bytes; the capacity is %d bytes", tooLarge.Bytes, tooLarge.Capacity),
		"purge issues the project no longer needs (kata purge); moving issues keeps their history here, and the signed receipts of purged events remain", nil)
}

// requireRelayDownstreamAuthority lets only the account that connected this
// relay re-share the project downstream. Every member of a relay may enroll a
// leaf, but a serve-downstream grant turns the enrolling daemon into another
// hub for the project, which is the connector's decision.
func requireRelayDownstreamAuthority(ctx context.Context, store db.Storage, projectID int64, actor string) error {
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		// The handshake reports a missing binding with its own error.
		return nil
	}
	if err != nil {
		return relayTransportError(err)
	}
	if binding.Role == db.FederationRoleSpoke && binding.RelayConfig != nil && binding.RelayConfig.LocalActor != actor {
		return api.NewError(http.StatusForbidden, "relay_downstream_forbidden", "only the account that connected this relay can enroll a downstream relay", "enroll without serve_downstream to receive the project as a leaf", nil)
	}
	return nil
}

func relayHandshake(ctx context.Context, store db.Storage, projectID int64, peer string, serveDownstream bool) (*api.RelayHandshake, error) {
	project, err := activeProjectByID(ctx, store, projectID)
	if err != nil {
		return nil, err
	}
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, federationError(err)
		}
		return nil, relayTransportError(err)
	}
	pin, err := store.RootAuthority(ctx, project.UID)
	if signer, _, configured := db.RootAttributionFromContext(ctx); configured && binding.Role == db.FederationRoleHub {
		if signer.AuthorityUID != store.InstanceUID() || len(signer.PrivateKey) != ed25519.PrivateKeySize {
			return nil, api.NewError(http.StatusConflict, "root_signing_key_conflict", "root signing key is unavailable", "restore federation-root.key from backup and restart the daemon, or run kata federation root-key repin --project "+project.Name, nil)
		}
		public := signer.PrivateKey.Public().(ed25519.PublicKey)
		expected := db.RootKeyPin{ProjectUID: project.UID, AuthorityUID: store.InstanceUID(), KeyID: db.RootPublicKeyID(public), PublicKey: public}
		if err == nil && (pin.AuthorityUID != expected.AuthorityUID || pin.KeyID != expected.KeyID) {
			return nil, api.NewError(http.StatusConflict, "root_signing_key_conflict", "root pin differs from the loaded signing key", "restore the pinned federation-root.key and restart the daemon, or run kata federation root-key repin --project "+project.Name, nil)
		}
		if errors.Is(err, db.ErrNotFound) && binding.Enabled {
			// The normal daemon already loaded its owner-only signing key. A
			// project's first enrollment may precede its first signed mutation.
			// Persist the public pin through the existing policy-fenced store;
			// never replace a retained pin or generate another private key here.
			if err := store.PinRootAuthority(ctx, expected); err != nil {
				return nil, relayTransportError(err)
			}
			pin, err = store.RootAuthority(ctx, project.UID)
		}
	}
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, federationError(err)
		}
		return nil, relayTransportError(err)
	}
	path := []string{store.InstanceUID()}
	if binding.Role == db.FederationRoleSpoke && binding.RelayConfig != nil {
		c := binding.RelayConfig
		if err := c.Validate(store.InstanceUID()); err != nil || !c.ServeDownstream || c.UpstreamRevoked || !binding.PushEnabled || c.AuthorityUID != pin.AuthorityUID {
			return nil, projectAccessDenied()
		}
		path = slices.Clone(c.HubPath)
	} else if binding.Role != db.FederationRoleHub || pin.AuthorityUID != store.InstanceUID() {
		return nil, projectAccessDenied()
	}
	if !binding.Enabled || peer == store.InstanceUID() || slices.Contains(path, peer) || len(path) > db.MaxRelayHubs || (serveDownstream && len(path) == db.MaxRelayHubs) {
		return nil, api.NewError(http.StatusBadRequest, "validation", "relay peer would exceed the hub limit or create a cycle", "", nil)
	}
	history, err := store.RootKeyTransitions(ctx, project.UID)
	if err != nil {
		return nil, relayTransportError(err)
	}
	transitions, err := activeRootKeyChain(pin, history)
	if err != nil {
		return nil, relayTransportError(err)
	}
	producer, err := db.ProjectEmbeddingProducerFromMetadata(project.Metadata)
	if err != nil {
		// Invalid retained producer configuration defers embedding generation,
		// but must not prevent enrollment or ordinary project synchronization.
		producer = nil
	}
	handshake := &api.RelayHandshake{EmbeddingProducer: producer, ProtocolVersion: db.RelayProtocolVersion, UpstreamInstanceUID: store.InstanceUID(), HubPath: append(path, peer), Root: pin, RootKeyTransitions: transitions}
	if len(transitions) > 0 {
		earliest := transitions[0].PreviousKeyID
		// The trusted initial enrollment supplies the oldest public pin. Existing
		// signed transitions retain every later key using normal rotation storage.
		for record, err := range store.ExportAttribution(ctx, db.ExportFilter{ProjectID: &projectID}) {
			if err != nil {
				return nil, relayTransportError(err)
			}
			candidate, ok := record.(*db.RootKeyPin)
			if ok && candidate.KeyID == earliest && candidate.ProjectUID == project.UID && candidate.AuthorityUID == pin.AuthorityUID {
				initialPin := *candidate
				handshake.EnrollmentRoot = &initialPin
				break
			}
		}
		if handshake.EnrollmentRoot == nil {
			return nil, relayTransportError(db.ErrFederationIngestValidation)
		}
	}
	return handshake, nil
}

// activeRootKeyChain returns the signed transitions that lead to the active
// pin, oldest first. An unsigned repin starts a new chain, so transitions
// before it stay retained for verifying old receipts but are not offered as an
// enrollment path.
func activeRootKeyChain(pin db.RootKeyPin, history []db.RootKeyTransition) ([]db.RootKeyTransition, error) {
	byNext := make(map[string]db.RootKeyTransition, len(history))
	for _, transition := range history {
		if _, exists := byNext[transition.Next.KeyID]; exists {
			return nil, db.ErrFederationIngestValidation
		}
		byNext[transition.Next.KeyID] = transition
	}
	var chain []db.RootKeyTransition
	for keyID := pin.KeyID; ; {
		transition, exists := byNext[keyID]
		if !exists {
			break
		}
		if len(chain) == len(history) {
			return nil, db.ErrFederationIngestValidation
		}
		chain = append(chain, transition)
		keyID = transition.PreviousKeyID
	}
	slices.Reverse(chain)
	return chain, nil
}

func relayEnrollmentNeedsReset(ctx context.Context, store db.Storage, bindingUID string) (bool, error) {
	capability, ok := store.(db.RelayResetBootstrapStore)
	if !ok {
		return false, db.ErrTransactionFinalizationFailed
	}
	return capability.RelayEnrollmentNeedsReset(ctx, bindingUID)
}
