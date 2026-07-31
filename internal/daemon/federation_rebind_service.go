package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

const (
	federationRebindRequestTimeout = 15 * time.Second
	federationRebindResponseLimit  = 1 << 20
)

var (
	// ErrFederationReplicaHubUnavailable classifies a target transport or
	// server failure without exposing the remote response body.
	ErrFederationReplicaHubUnavailable = errors.New("federation hub unavailable")

	newFederationRebindHTTPClient = func(
		_ context.Context, baseURL, token string,
	) (*http.Client, error) {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, errors.New("default HTTP transport is unavailable")
		}
		httpClient := &http.Client{
			Transport: transport.Clone(),
			Timeout:   federationRebindRequestTimeout,
		}
		if err := config.ConfigureBearerClientWithTrust(httpClient, baseURL, token, false); err != nil {
			return nil, err
		}
		return httpClient, nil
	}
)

// FederationRebindState describes whether one call performed the whole local
// transition, resumed a half-migrated transition, or validated a target that
// was already fully converged.
type FederationRebindState string

const (
	FederationRebindStateRebound   FederationRebindState = "rebound"
	FederationRebindStateResumed   FederationRebindState = "resumed"
	FederationRebindStateUnchanged FederationRebindState = "unchanged"
)

// FederationRebindMetadataFetcher validates the replacement endpoint with the
// existing project enrollment token.
type FederationRebindMetadataFetcher func(
	context.Context, string, string, int64,
) (api.ProjectFederationBody, error)

// RebindFederationReplicaParams selects one spoke and one server-owned catalog
// entry. The request-facing API supplies the catalog entry by name; it never
// supplies a raw URL or credential directly.
type RebindFederationReplicaParams struct {
	ProjectID     int64
	HubCatalog    config.CatalogDaemonConfig
	FetchMetadata FederationRebindMetadataFetcher
}

// RebindFederationReplicaResult reports the converged local state.
type RebindFederationReplicaResult struct {
	Project        db.Project
	Binding        db.FederationBinding
	PreviousHubURL string
	State          FederationRebindState
}

type federationRebindLocalState struct {
	credentialTarget bool
	bindingTarget    bool
	sourceHubURL     string
	sourceInsecure   bool
}

// RebindFederationReplica validates a catalog-owned HTTPS target against the
// existing enrollment before converging credential and binding endpoint state.
func RebindFederationReplica(
	ctx context.Context,
	store db.Storage,
	credentials config.FederationCredentialStore,
	p RebindFederationReplicaParams,
) (RebindFederationReplicaResult, error) {
	targetURL, err := validateFederationRebindCatalog(p.HubCatalog)
	if err != nil {
		return RebindFederationReplicaResult{}, err
	}
	if p.ProjectID <= 0 {
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, "project_id is required", "",
		)
	}
	if credentials == nil {
		return RebindFederationReplicaResult{}, credentialIOError(
			"read federation credential before rebind",
		)
	}
	replacer, ok := credentials.(config.FederationCredentialReplacer)
	if !ok {
		return RebindFederationReplicaResult{}, credentialIOError(
			"credential store lacks exact replacement support",
		)
	}

	ensureFederationReplicaMu.Lock()
	project, err := store.ProjectByID(ctx, p.ProjectID)
	if err != nil {
		ensureFederationReplicaMu.Unlock()
		if errors.Is(err, db.ErrNotFound) {
			return RebindFederationReplicaResult{}, federationReplicaError(
				errFederationReplicaProjectNotFound,
				"federation replica project was not found",
				"",
			)
		}
		return RebindFederationReplicaResult{}, fmt.Errorf(
			"read federation replica project before rebind: %w", err,
		)
	}
	key := federationReplicaTransitionKey(store, project.Name)
	if _, pending := federationReplicaLeaveIntents[key]; pending || federationReplicaIsSuppressed(key) {
		ensureFederationReplicaMu.Unlock()
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaLeavePending,
			"explicit federation leave is pending",
			"",
		)
	}
	binding, err := store.FederationBindingByProject(ctx, p.ProjectID)
	if err != nil {
		ensureFederationReplicaMu.Unlock()
		if errors.Is(err, db.ErrNotFound) {
			return RebindFederationReplicaResult{}, federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"project has no federation binding",
				"",
			)
		}
		return RebindFederationReplicaResult{}, fmt.Errorf(
			"read federation binding before rebind: %w", err,
		)
	}
	if binding.Role != db.FederationRoleSpoke {
		ensureFederationReplicaMu.Unlock()
		return RebindFederationReplicaResult{}, db.ErrFederationNotSpoke
	}
	credential, found, err := credentials.FederationCredential(ctx, project.UID)
	if err != nil {
		ensureFederationReplicaMu.Unlock()
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return RebindFederationReplicaResult{}, federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"federation credential changed before rebind",
				"",
			)
		}
		return RebindFederationReplicaResult{}, credentialIOError(
			"read federation credential before rebind",
		)
	}
	if !found || strings.TrimSpace(credential.Token) == "" {
		ensureFederationReplicaMu.Unlock()
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"federation enrollment credential is missing",
			"",
		)
	}
	if err := validateFederationRebindIdentity(project, binding, credential, p.HubCatalog.Name); err != nil {
		ensureFederationReplicaMu.Unlock()
		return RebindFederationReplicaResult{}, err
	}
	localState, err := classifyFederationRebindState(binding, credential, targetURL)
	if err != nil {
		ensureFederationReplicaMu.Unlock()
		return RebindFederationReplicaResult{}, err
	}
	targetCredential := credential
	targetCredential.HubURL = targetURL
	targetCredential.AllowInsecure = false
	finishOperation := registerFederationReplicaHubOperationLocked(key)
	ensureFederationReplicaMu.Unlock()
	defer finishOperation()

	fetchMetadata := p.FetchMetadata
	if fetchMetadata == nil {
		fetchMetadata = fetchFederationRebindMetadata
	}
	metadata, err := fetchMetadata(
		ctx, targetURL, credential.Token, binding.HubProjectID,
	)
	if err != nil {
		return RebindFederationReplicaResult{}, err
	}
	if metadata.ProjectID != binding.HubProjectID || metadata.ProjectUID != binding.HubProjectUID {
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"replacement endpoint returned a different federation project identity",
			"use federation leave and join for a different hub project",
		)
	}

	ensureFederationReplicaMu.Lock()
	defer ensureFederationReplicaMu.Unlock()
	if _, pending := federationReplicaLeaveIntents[key]; pending || federationReplicaIsSuppressed(key) {
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaLeavePending,
			"explicit federation leave began during rebind validation",
			"retry after resolving the leave operation",
		)
	}
	currentCredential, found, err := credentials.FederationCredential(ctx, project.UID)
	if err != nil {
		return RebindFederationReplicaResult{}, credentialIOError(
			"reread federation credential after rebind validation",
		)
	}
	if !found || currentCredential.LeavePending {
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaLeavePending,
			"federation credential is pending leave",
			"",
		)
	}
	if currentCredential != credential && currentCredential != targetCredential {
		return RebindFederationReplicaResult{}, federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"federation credential changed during rebind validation",
			"retry with the current credential state",
		)
	}
	currentBinding, err := store.FederationBindingByProject(ctx, project.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return RebindFederationReplicaResult{}, federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"federation binding disappeared during rebind validation",
				"",
			)
		}
		return RebindFederationReplicaResult{}, fmt.Errorf(
			"reread federation binding after rebind validation: %w", err,
		)
	}
	if err := validateFederationRebindBindingState(
		currentBinding, binding, localState, targetURL,
	); err != nil {
		return RebindFederationReplicaResult{}, err
	}
	if err := replacer.ReplaceFederationCredential(ctx, config.FederationCredentialReplacement{
		ProjectUID: project.UID, Expected: credential, Replacement: targetCredential,
	}); err != nil {
		if errors.Is(err, config.ErrFederationCredentialConflict) {
			return RebindFederationReplicaResult{}, federationReplicaError(
				ErrFederationReplicaCredentialConflict,
				"federation credential changed before replacement",
				"retry with the current credential state",
			)
		}
		return RebindFederationReplicaResult{}, credentialIOError(
			"replace federation credential during rebind",
		)
	}
	rebound, err := store.RebindFederationBinding(ctx, db.RebindFederationBindingParams{
		ProjectID:             project.ID,
		ExpectedHubURL:        localState.sourceHubURL,
		ExpectedAllowInsecure: localState.sourceInsecure,
		HubProjectID:          binding.HubProjectID,
		HubProjectUID:         binding.HubProjectUID,
		TargetHubURL:          targetURL,
	})
	if err != nil {
		switch {
		case errors.Is(err, db.ErrFederationRebindConflict):
			return RebindFederationReplicaResult{}, federationReplicaError(
				ErrFederationReplicaBindingConflict,
				"federation binding changed before replacement",
				"retry with the current binding state",
			)
		case errors.Is(err, db.ErrFederationNotSpoke):
			return RebindFederationReplicaResult{}, db.ErrFederationNotSpoke
		default:
			return RebindFederationReplicaResult{}, fmt.Errorf(
				"replace federation binding during rebind: %w", err,
			)
		}
	}

	state := FederationRebindStateRebound
	if localState.credentialTarget && localState.bindingTarget {
		state = FederationRebindStateUnchanged
	} else if localState.credentialTarget || localState.bindingTarget {
		state = FederationRebindStateResumed
	}
	return RebindFederationReplicaResult{
		Project: project, Binding: rebound,
		PreviousHubURL: localState.sourceHubURL,
		State:          state,
	}, nil
}

func validateFederationRebindCatalog(catalog config.CatalogDaemonConfig) (string, error) {
	if strings.TrimSpace(catalog.Name) == "" {
		return "", federationReplicaError(
			ErrFederationReplicaInvalidInput, "hub catalog name is required", "",
		)
	}
	if catalog.Local {
		return "", federationReplicaError(
			ErrFederationReplicaInvalidInput, "hub catalog entry must be remote", "",
		)
	}
	normalized, err := normalizeFederationHubBaseURL(catalog.URL)
	if err != nil {
		return "", federationReplicaError(
			ErrFederationReplicaInvalidInput, "hub catalog URL is invalid", "",
		)
	}
	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme != "https" {
		return "", federationReplicaError(
			ErrFederationReplicaInvalidInput,
			"federation rebind requires an HTTPS hub catalog URL",
			"",
		)
	}
	return normalized, nil
}

func validateFederationRebindIdentity(
	project db.Project,
	binding db.FederationBinding,
	credential config.FederationCredential,
	catalogName string,
) error {
	if binding.HubProjectID <= 0 || binding.HubProjectUID == "" ||
		binding.HubProjectUID != project.UID || credential.HubProjectID != binding.HubProjectID {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"local federation identity is inconsistent",
			"use federation leave and join to repair project identity",
		)
	}
	if credential.LeavePending {
		return federationReplicaError(
			ErrFederationReplicaLeavePending,
			"federation credential is pending leave",
			"",
		)
	}
	if credential.ManagedByConfig && strings.TrimSpace(credential.HubCatalog) != strings.TrimSpace(catalogName) {
		return federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"config-managed federation credential belongs to another hub catalog",
			"select the credential's existing hub catalog or update the declarative mapping",
		)
	}
	return nil
}

func classifyFederationRebindState(
	binding db.FederationBinding,
	credential config.FederationCredential,
	targetURL string,
) (federationRebindLocalState, error) {
	bindingURL, err := normalizeFederationHubBaseURL(binding.HubURL)
	if err != nil {
		return federationRebindLocalState{}, federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"existing federation binding has an invalid hub URL",
			"",
		)
	}
	credentialURL, err := normalizeFederationHubBaseURL(credential.HubURL)
	if err != nil {
		return federationRebindLocalState{}, federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"existing federation credential has an invalid hub URL",
			"",
		)
	}
	state := federationRebindLocalState{
		bindingTarget:    bindingURL == targetURL && !binding.AllowInsecure,
		credentialTarget: credentialURL == targetURL && !credential.AllowInsecure,
	}
	if !state.bindingTarget {
		state.sourceHubURL = binding.HubURL
		state.sourceInsecure = binding.AllowInsecure
	}
	if !state.credentialTarget {
		if state.sourceHubURL == "" {
			state.sourceHubURL = credential.HubURL
			state.sourceInsecure = credential.AllowInsecure
		} else {
			sourceURL, sourceErr := normalizeFederationHubBaseURL(state.sourceHubURL)
			if sourceErr != nil || sourceURL != credentialURL ||
				state.sourceInsecure != credential.AllowInsecure {
				return federationRebindLocalState{}, federationReplicaError(
					ErrFederationReplicaBindingConflict,
					"binding and credential disagree on the current federation endpoint",
					"resolve the conflicting local state before rebind",
				)
			}
		}
	}
	if state.sourceHubURL == "" {
		state.sourceHubURL = targetURL
		state.sourceInsecure = false
	}
	return state, nil
}

func validateFederationRebindBindingState(
	current db.FederationBinding,
	snapshot db.FederationBinding,
	state federationRebindLocalState,
	targetURL string,
) error {
	if current.Role != db.FederationRoleSpoke {
		return db.ErrFederationNotSpoke
	}
	if current.ProjectID != snapshot.ProjectID ||
		current.HubProjectID != snapshot.HubProjectID ||
		current.HubProjectUID != snapshot.HubProjectUID {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"federation binding identity changed during rebind validation",
			"",
		)
	}
	normalized, err := normalizeFederationHubBaseURL(current.HubURL)
	if err != nil {
		return federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"federation binding endpoint became invalid during rebind validation",
			"",
		)
	}
	if normalized == targetURL && !current.AllowInsecure {
		return nil
	}
	sourceURL, _ := normalizeFederationHubBaseURL(state.sourceHubURL)
	if normalized == sourceURL && current.AllowInsecure == state.sourceInsecure {
		return nil
	}
	return federationReplicaError(
		ErrFederationReplicaBindingConflict,
		"federation binding endpoint changed during rebind validation",
		"retry with the current binding state",
	)
}

func registerFederationReplicaHubOperationLocked(key string) func() {
	state := federationReplicaHubOperations[key]
	if state == nil {
		state = &federationReplicaHubOperationState{done: make(chan struct{})}
		federationReplicaHubOperations[key] = state
	}
	state.count++
	var once sync.Once
	return func() {
		once.Do(func() {
			ensureFederationReplicaMu.Lock()
			defer ensureFederationReplicaMu.Unlock()
			state := federationReplicaHubOperations[key]
			if state == nil {
				return
			}
			state.count--
			if state.count == 0 {
				delete(federationReplicaHubOperations, key)
				close(state.done)
			}
		})
	}
}

func fetchFederationRebindMetadata(
	ctx context.Context,
	hubURL string,
	token string,
	hubProjectID int64,
) (api.ProjectFederationBody, error) {
	httpClient, err := newFederationRebindHTTPClient(ctx, hubURL, token)
	if err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaHubUnavailable,
			"configure replacement hub transport",
			"check the HTTPS catalog endpoint",
		)
	}
	if err := configureFederationRebindRedirects(httpClient, hubURL); err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaHubUnavailable,
			"configure replacement hub redirect policy",
			"check the HTTPS catalog endpoint",
		)
	}
	requestURL, err := url.JoinPath(
		hubURL,
		fmt.Sprintf("/api/v1/projects/%d/federation/metadata", hubProjectID),
	)
	if err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, "build replacement hub metadata URL", "",
		)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaInvalidInput, "build replacement hub metadata request", "",
		)
	}
	response, err := httpClient.Do(request) //nolint:gosec // target is the operator-selected daemon catalog entry.
	if err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaHubUnavailable,
			"replacement hub metadata request failed",
			"check the HTTPS catalog endpoint and retry",
		)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaCredentialConflict,
			"replacement hub rejected the existing federation enrollment",
			"verify this catalog entry points to the same hub",
		)
	}
	if response.StatusCode == http.StatusNotFound {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaBindingConflict,
			"replacement hub does not contain the bound federation project",
			"verify this catalog entry points to the same hub",
		)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaHubUnavailable,
			"replacement hub metadata request returned an error",
			"check the HTTPS catalog endpoint and retry",
		)
	}
	var metadata api.ProjectFederationBody
	decoder := json.NewDecoder(io.LimitReader(response.Body, federationRebindResponseLimit))
	if err := decoder.Decode(&metadata); err != nil {
		return api.ProjectFederationBody{}, federationReplicaError(
			ErrFederationReplicaHubUnavailable,
			"replacement hub returned invalid federation metadata",
			"check the HTTPS catalog endpoint and retry",
		)
	}
	return metadata, nil
}

func configureFederationRebindRedirects(httpClient *http.Client, baseURL string) error {
	if httpClient == nil {
		return errors.New("cannot configure redirects on a nil HTTP client")
	}
	origin, err := config.CanonicalHTTPOrigin(baseURL)
	if err != nil {
		return err
	}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		requestOrigin, err := config.CanonicalHTTPOrigin(request.URL.String())
		if err != nil || requestOrigin != origin {
			return errors.New("redirect crossed the configured HTTP origin")
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return nil
}
