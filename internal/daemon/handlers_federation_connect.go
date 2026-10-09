package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/uid"
	"go.kenn.io/kata/pkg/client/generated"
)

func registerFederationBridgeConnect(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{OperationID: "connectFederationBridge", Method: http.MethodPost, Path: "/api/v1/federation/bridges", Summary: "Connect one project to a configured hub"}, func(ctx context.Context, in *api.ConnectFederationBridgeRequest) (*api.ConnectFederationBridgeResponse, error) {
		actor, err := attributedActor(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if db.ValidateTokenActor(actor) != nil || config.ValidateProjectName(in.Body.ProjectName) != nil || strings.TrimSpace(in.Body.HubProject) == "" {
			return nil, api.NewError(400, "validation", "valid local account and project names are required", "", nil)
		}
		catalog, err := federationRebindCatalogByName(cfg.FederationCatalog, in.Body.HubCatalog)
		if err != nil {
			return nil, err
		}
		hubURL, err := httpurl.CanonicalHTTPOrigin(catalog.URL)
		if err != nil || strings.TrimRight(strings.TrimSpace(catalog.URL), "/") != hubURL || catalog.Local {
			return nil, api.NewError(400, "validation", "hub_catalog must select a remote root HTTP(S) origin", "", nil)
		}
		token := strings.TrimSpace(catalog.Token)
		if catalog.TokenEnv != "" {
			token = strings.TrimSpace(os.Getenv(catalog.TokenEnv))
		}
		if token == "" {
			return nil, api.NewError(400, "hub_credential_required", "selected hub has no user credential", "", nil)
		}
		// Pin both the authorization header and secret-bearing enrollment body.
		// No redirect is needed for the canonical root API paths.
		httpClient := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		policy := config.BearerPolicy{AllowInsecurePlaintext: catalog.AllowInsecure}
		if err := policy.ConfigureClient(httpClient, hubURL, token); err != nil {
			return nil, api.NewError(400, "validation", "hub credential target violates transport policy", "", nil)
		}
		bridge, err := newFederationBridgeAPIClient(httpClient, hubURL)
		if err != nil {
			return nil, internalAPIError(err)
		}
		instanceResponse, callErr := bridge.InstanceWithResponse(ctx)
		var instanceRaw []byte
		if instanceResponse != nil {
			instanceRaw = instanceResponse.Body
		}
		var instance api.InstanceResponse
		if err := decodeFederationBridgeResponse(instanceRaw, callErr, &instance.Body); err != nil {
			return nil, err
		}
		peer := instance.Body
		if peer.RelayProtocolVersion != db.RelayProtocolVersion || peer.ProvenanceProtocolVersion != 1 || peer.EmbeddingArtifactProtocolVersion != 1 {
			return nil, api.NewError(409, "unsupported_relay_protocol", "hub does not support the required relay, provenance and embedding artifact protocols", "upgrade the selected hub before connecting", nil)
		}
		if !uid.Valid(peer.InstanceUID) || peer.InstanceUID == cfg.DB.InstanceUID() || (catalog.InstanceUID != "" && catalog.InstanceUID != peer.InstanceUID) {
			return nil, api.NewError(409, "hub_identity_conflict", "selected hub identity does not match the catalog", "", nil)
		}
		if !peer.WebUICapabilities.Writable {
			return nil, api.NewError(http.StatusConflict, "hub_readonly", "selected hub account is read-only", "use a writable hub account for bidirectional relay", nil)
		}
		if peer.Auth.Kind != string(PrincipalDBToken) || peer.Auth.Scope != nil || db.ValidateTokenActor(peer.Auth.Actor) != nil {
			return nil, api.NewError(403, "relay_user_credential_required", "selected hub requires an ordinary account credential", "", nil)
		}
		resolvedResponse, callErr := bridge.ResolveProjectWithResponse(ctx, &generated.ResolveProjectRequestOptions{Body: &generated.ResolveProjectBody{Name: &in.Body.HubProject}})
		var resolvedRaw []byte
		if resolvedResponse != nil {
			resolvedRaw = resolvedResponse.Body
		}
		var resolved api.ProjectResolveBody
		if err := decodeFederationBridgeResponse(resolvedRaw, callErr, &resolved); err != nil {
			return nil, err
		}
		metadataResponse, callErr := bridge.GetProjectFederationWithResponse(ctx, &generated.GetProjectFederationRequestOptions{PathParams: &generated.GetProjectFederationPath{ProjectID: resolved.Project.ID}})
		var metadataRaw []byte
		if metadataResponse != nil {
			metadataRaw = metadataResponse.Body
		}
		var metadata api.ProjectFederationBody
		if err := decodeFederationBridgeResponse(metadataRaw, callErr, &metadata); err != nil {
			return nil, err
		}
		if metadata.ProjectID != resolved.Project.ID || metadata.ProjectUID != resolved.Project.UID || !uid.Valid(metadata.ProjectUID) || metadata.ReplayHorizonEventID <= 0 {
			return nil, api.NewError(409, "hub_project_conflict", "hub returned inconsistent project metadata", "", nil)
		}
		if err := federationBridgeProjectCollision(ctx, cfg.DB, metadata.ProjectUID, in.Body.ProjectName); err != nil {
			return nil, err
		}
		body := api.FederationBridgeBody{HubCatalog: catalog.Name, HubURL: hubURL, HubInstanceUID: peer.InstanceUID, HubProjectUID: metadata.ProjectUID, HubProjectID: metadata.ProjectID, ProjectName: in.Body.ProjectName, LocalAccount: actor, UpstreamAccount: peer.Auth.Actor, Direction: "bidirectional", Status: "ready"}
		if in.Body.Preflight {
			return &api.ConnectFederationBridgeResponse{Body: body}, nil
		}
		credentials := cfg.federationCredentialStore()
		credential, err := reserveFederationBridgeCredential(ctx, cfg.DB, credentials, body, catalog.AllowInsecure)
		if err != nil {
			return nil, err
		}
		finishEnrollment, err := beginFederationBridgeEnrollment(ctx, cfg.DB, credentials, in.Body.ProjectName, metadata.ProjectUID, credential)
		if err != nil {
			return nil, err
		}
		// An explicit connect may rebind the retained narrow grant to the selected account credential.
		rebindParent := true
		grantResponse, callErr := bridge.CreateFederationEnrollmentWithResponse(ctx, &generated.CreateFederationEnrollmentRequestOptions{Body: &generated.CreateFederationEnrollmentBody{
			ProjectID: metadata.ProjectID, SpokeInstanceUID: cfg.DB.InstanceUID(), Capabilities: "claim,pull,push", Token: &credential.Token,
			Relay: &generated.RelayEnrollmentOptions{ProtocolVersion: int64(db.RelayProtocolVersion), ServeDownstream: in.Body.ServeDownstream, RebindParent: &rebindParent},
		}})
		// Drain only the remote enrollment request. Local setup has its own
		// leave-state revalidation around the project transport gate; holding
		// this drain through that gate would deadlock a leave waiting to proceed.
		finishEnrollment()
		var grantRaw []byte
		if grantResponse != nil {
			grantRaw = grantResponse.Body
		}
		var grant api.FederationEnrollmentOut
		if err := decodeFederationBridgeResponse(grantRaw, callErr, &grant); err != nil {
			return nil, err
		}
		if grant.Relay == nil || grant.Actor != peer.Auth.Actor || grant.SpokeInstanceUID != cfg.DB.InstanceUID() || grant.ProjectID == nil || *grant.ProjectID != metadata.ProjectID || grant.Token != credential.Token || grant.Capabilities != "claim,pull,push" || grant.Relay.UpstreamInstanceUID != peer.InstanceUID || grant.Relay.Root.ProjectUID != metadata.ProjectUID {
			return nil, api.NewError(409, "invalid_relay_grant", "hub returned an inconsistent relay grant", "retry with the same selected account and project", nil)
		}
		expectedCredential := credential
		credential.RelayEnrollmentPending = false
		result, err := EnsureFederationReplica(ctx, cfg.DB, credentials, cfg.FederationWake, EnsureFederationReplicaParams{
			HubURL: hubURL, HubProjectID: metadata.ProjectID, HubProjectUID: metadata.ProjectUID,
			ProjectName: in.Body.ProjectName, ReplayHorizonEventID: metadata.ReplayHorizonEventID,
			Credential: credential, ExpectedCredential: &expectedCredential, PushEnabled: true,
			Relay: grant.Relay, RelayLocalActor: actor, RelayServeDownstream: in.Body.ServeDownstream,
			ProjectEventSink: func(event db.Event) { deliverProjectMutation(cfg, &event) },
		})
		if err != nil {
			return nil, federationReplicaAPIError(err)
		}
		body.Status = "connected"
		body.ProjectID = result.Project.ID
		return &api.ConnectFederationBridgeResponse{Body: body}, nil
	})
}

func beginFederationBridgeEnrollment(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	projectName, projectUID string,
	expected config.FederationCredential,
) (func(), error) {
	key := federationReplicaOperationKey(store, projectName, expected)
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	state := federationReplicaTransitions.state(key)
	if state == federationReplicaLeavePending {
		return nil, federationReplicaAPIError(federationReplicaTransitions.leaveBlockedError(key))
	}
	current, found, err := credentials.FederationCredential(ctx, projectUID)
	if err != nil {
		return nil, api.NewError(http.StatusServiceUnavailable, "federation_credentials_unavailable", "cannot read bridge credential before enrollment", "retry the same bridge connection", nil)
	}
	if !found || !current.Equal(expected) {
		return nil, api.NewError(http.StatusConflict, "federation_credential_conflict", "bridge credential changed before enrollment", "retry after resolving the current bridge state", nil)
	}
	if state == federationReplicaLeft {
		// An explicit connect is the operator action that rejoins a completed
		// leave. Clear that terminal state only after the fresh reservation was
		// verified, then register the hub request under the same mutex.
		federationReplicaTransitions.clearLeave(key)
	}
	finish := federationReplicaTransitions.registerHubOperationLocked(key)
	return func() {
		ensureFederationReplicaMu.Lock()
		defer ensureFederationReplicaMu.Unlock()
		finish()
	}, nil
}

func federationBridgeProjectCollision(ctx context.Context, store db.Storage, projectUID, name string) error {
	project, err := store.ProjectByNameIncludingArchived(ctx, name)
	if err == nil && project.UID != projectUID {
		return api.NewError(409, "project_name_collision", "local project name belongs to another project", "choose a different local project name", nil)
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return internalAPIError(err)
	}
	project, err = store.ProjectByUID(ctx, projectUID)
	if err == nil && (project.Name != name || project.DeletedAt != nil) {
		return api.NewError(409, "project_identity_collision", "shared project already exists under another name or is archived", "resolve the existing local project before connecting", nil)
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return internalAPIError(err)
	}
	return nil
}

func federationBridgePendingCredentialCollision(
	ctx context.Context,
	credentials config.FederationCredentialStore,
	projectUID, name string,
) error {
	reader, ok := credentials.(config.FederationRelayPendingMetadataReader)
	if !ok {
		return api.NewError(http.StatusServiceUnavailable, "federation_credentials_unavailable", "credential storage cannot check pending bridge names", "configure pending bridge metadata lookup before connecting", nil)
	}
	pendingUID, _, found, err := reader.PendingRelayCredentialMetadata(ctx, name)
	if errors.Is(err, config.ErrFederationCredentialConflict) {
		return api.NewError(409, "federation_credential_conflict", "multiple pending bridge credentials reserve this local project name", "resolve pending bridge setup before connecting", nil)
	}
	if err != nil {
		return api.NewError(http.StatusServiceUnavailable, "federation_credentials_unavailable", "cannot check pending bridge names before enrollment", "retry after credential storage is available", nil)
	}
	if found && pendingUID != projectUID {
		return api.NewError(409, "federation_credential_conflict", "another pending bridge enrollment reserves this local project name", "resolve pending bridge setup before connecting to a different hub project", nil)
	}
	return nil
}

func reserveFederationBridgeCredential(ctx context.Context, store db.Storage, credentials config.FederationCredentialStore, body api.FederationBridgeBody, allowInsecure bool) (config.FederationCredential, error) {
	if credentials == nil {
		return config.FederationCredential{}, api.NewError(503, "federation_credentials_unavailable", "federation credential storage is unavailable", "", nil)
	}
	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	key := federationReplicaTransitionKey(store, body.ProjectName)
	if federationReplicaTransitions.state(key) == federationReplicaLeavePending {
		return config.FederationCredential{}, federationReplicaAPIError(federationReplicaTransitions.leaveBlockedError(key))
	}
	if err := federationBridgeProjectCollision(ctx, store, body.HubProjectUID, body.ProjectName); err != nil {
		return config.FederationCredential{}, err
	}
	if err := federationBridgePendingCredentialCollision(
		ctx, credentials, body.HubProjectUID, body.ProjectName,
	); err != nil {
		return config.FederationCredential{}, err
	}
	current, found, err := credentials.FederationCredential(ctx, body.HubProjectUID)
	if err != nil {
		return config.FederationCredential{}, internalAPIError(err)
	}
	if found {
		if current.LeavePending || current.Provider != nil || current.HubURL != body.HubURL || current.HubProjectID != body.HubProjectID || current.Actor != body.UpstreamAccount || current.HubCatalog != body.HubCatalog || current.SpokeProjectName != body.ProjectName || current.RequestedActor != body.LocalAccount || current.AllowInsecure != allowInsecure || current.Capabilities != "claim,pull,push" || current.Token == "" {
			return config.FederationCredential{}, api.NewError(409, "federation_credential_conflict", "existing project credential differs from the selected bridge", "resolve the existing binding before connecting", nil)
		}
		return current, nil
	}
	token, err := newPlaintextToken()
	if err != nil {
		return config.FederationCredential{}, internalAPIError(err)
	}
	current = config.FederationCredential{HubURL: body.HubURL, HubProjectID: body.HubProjectID, Token: token, Capabilities: "claim,pull,push", Actor: body.UpstreamAccount, AllowInsecure: allowInsecure, HubCatalog: body.HubCatalog, RequestedActor: body.LocalAccount, SpokeProjectName: body.ProjectName, RelayEnrollmentPending: true}
	if err := credentials.StoreFederationCredential(ctx, body.HubProjectUID, current); err != nil {
		return config.FederationCredential{}, api.NewError(503, "federation_credentials_unavailable", "cannot retain enrollment retry identity", "", nil)
	}
	return current, nil
}

// Use the existing bounded JSON transport conventions; never echo upstream
// response bodies because they can contain credentials or private diagnostics.
// Generated routes reuse the already origin-pinned, redirect-rejecting client.
// The doer retains the original response bounds and performs one HTTP attempt.
func newFederationBridgeAPIClient(client *http.Client, baseURL string) (*generated.Client, error) {
	return generated.NewDefaultClient(baseURL, runtime.WithHTTPClient(federationBridgeDoer{client}))
}

type federationBridgeDoer struct{ client *http.Client }

func (d federationBridgeDoer) Do(ctx context.Context, request *http.Request) (*http.Response, error) {
	response, err := d.client.Do(request.WithContext(ctx)) //nolint:gosec // G704: generated routes use the explicitly configured, origin-pinned hub client.
	if err != nil {
		return nil, api.NewError(503, "hub_unavailable", "selected hub request failed", "retry the same bridge connection", nil)
	}
	originalBody := response.Body
	defer func() { _ = originalBody.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		if status >= 300 && status < 400 {
			status = 502
		}
		return nil, api.NewError(status, "hub_request_rejected", "selected hub rejected the request", "check account access and selected project", nil)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, api.NewError(502, "invalid_hub_response", "selected hub returned an invalid response", "", nil)
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	return response, nil
}

func decodeFederationBridgeResponse(raw []byte, callErr error, output any) error {
	if callErr != nil {
		if apiErr, ok := errors.AsType[*api.APIError](callErr); ok {
			return apiErr
		}
		return api.NewError(502, "invalid_hub_response", "selected hub returned an invalid response", "", nil)
	}
	if json.Unmarshal(raw, output) != nil {
		return api.NewError(502, "invalid_hub_response", "selected hub returned an invalid response", "", nil)
	}
	return nil
}
