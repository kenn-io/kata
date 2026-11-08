package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/kata/internal/agentsviewservice"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/httpurl"
)

func agentsViewUnavailable() error {
	return api.NewError(http.StatusServiceUnavailable, "transcript_service_unavailable", "configured AgentsView service is unavailable for this project and route", "no transcript was read or uploaded", nil)
}

func agentsViewPin(pin db.RootKeyPin) agentsviewservice.TrustedRoot {
	return agentsviewservice.TrustedRoot{ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, PublicKey: pin.PublicKey, Retired: pin.Retired}
}

const (
	agentsViewHeaderTimeout  = 2 * time.Second
	agentsViewMaxHeaderBytes = 16 << 10
	agentsViewRequestTimeout = 10 * time.Second
)

// withAgentsViewDiscoveryBudget starts the budget before host and federation
// authorization. ServeMux applies the same path and method matching as the API.
func withAgentsViewDiscoveryBudget(next http.Handler) http.Handler {
	matcher := http.NewServeMux()
	matcher.HandleFunc(http.MethodGet+" "+api.AgentsViewServicePath, func(http.ResponseWriter, *http.Request) {})
	matcher.HandleFunc(http.MethodGet+" "+api.FederationAgentsViewServicePath, func(http.ResponseWriter, *http.Request) {})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := matcher.Handler(r)
		if pattern == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), agentsViewRequestTimeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tryBeginAgentsViewTransport(ctx context.Context, cfg ServerConfig, projectID int64) (func(), error) {
	finish, err := federationcoord.TryBeginSync(
		ctx,
		federationcoord.Key(cfg.DB.InstanceUID(), projectID),
		cfg.DB,
		projectID,
	)
	if errors.Is(err, federationcoord.ErrBusy) || errors.Is(err, context.DeadlineExceeded) {
		return nil, agentsViewUnavailable()
	}
	if err != nil {
		return nil, internalAPIError(fmt.Errorf("coordinate AgentsView discovery: %w", err))
	}
	return finish, nil
}

// Discovery follows only the selected project's negotiated upstream. A root
// service entry on a relay never substitutes for unavailable upstream policy.
func discoverAgentsViewService(ctx context.Context, cfg ServerConfig, projectID int64) (agentsviewservice.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, agentsViewRequestTimeout)
	defer cancel()
	finish, err := tryBeginAgentsViewTransport(ctx, cfg, projectID)
	if err != nil {
		return agentsviewservice.Discovery{}, err
	}
	defer finish()
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return agentsviewservice.Discovery{}, err
	}
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	pin, err := cfg.DB.RootAuthority(ctx, project.UID)
	if err != nil || db.ValidateRootKeyPin(pin) != nil || pin.Retired {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	var result agentsviewservice.Discovery
	path := []string{cfg.DB.InstanceUID()}
	switch binding.Role {
	case db.FederationRoleHub:
		target := cfg.AgentsView.Project[project.UID]
		if !target.Enabled || pin.AuthorityUID != cfg.DB.InstanceUID() || cfg.RootSigning == nil {
			return result, agentsViewUnavailable()
		}
		signer := cfg.RootSigning.Signer()
		if !db.RootSignerMatches(pin, signer) {
			return result, agentsViewUnavailable()
		}
		serviceID, policyErr := agentsviewservice.ServiceID(target.ReceiverURL, target.LocatorURL)
		if policyErr != nil {
			return result, agentsViewUnavailable()
		}
		now := time.Now().UTC()
		descriptor, signErr := agentsviewservice.SignDescriptor(agentsviewservice.Descriptor{
			Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, ServiceID: serviceID,
			LocatorBase: target.LocatorURL, EnvelopeVersion: 1, MaxBytes: agentsviewservice.MaxSnapshotBytes,
			IssuedAt: now.Unix(), ExpiresAt: now.Add(agentsviewservice.MaxLifetime).Unix(),
		}, signer.PrivateKey)
		if signErr != nil {
			return result, agentsViewUnavailable()
		}
		result = agentsviewservice.Discovery{Descriptor: descriptor, InstanceUID: cfg.DB.InstanceUID(), Route: []agentsviewservice.RouteObservation{{InstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID}}}
	case db.FederationRoleSpoke:
		relay := binding.RelayConfig
		// Legacy cursor-only spokes have no negotiated route generation. Refuse
		// this new service rather than pretending they have the relay contract.
		if relay == nil || relay.UpstreamRevoked || relay.Validate(cfg.DB.InstanceUID()) != nil || relay.AuthorityUID != pin.AuthorityUID || binding.HubProjectUID != project.UID {
			return result, agentsViewUnavailable()
		}
		path = append([]string(nil), relay.HubPath...)
		credential, _, credentialErr := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
		if credentialErr != nil || credential.LeavePending || credential.NativeLeave != nil || credential.RelayEnrollmentPending || credential.Token == "" {
			return result, agentsViewUnavailable()
		}
		credentialURL, e1 := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
		bindingURL, e2 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
		if e1 != nil || e2 != nil || credentialURL != bindingURL || credential.HubProjectID != binding.HubProjectID {
			return result, agentsViewUnavailable()
		}
		result, err = fetchAgentsViewService(ctx, cfg, binding, credential, project.UID, pin, path[:len(path)-1], *relay, cfg.DB.InstanceUID())
		if err != nil {
			return agentsviewservice.Discovery{}, err
		}
		result.Route = append(result.Route, agentsviewservice.RouteObservation{InstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, BindingUID: relay.BindingUID, UpstreamInstanceUID: relay.UpstreamInstanceUID, ResetEpoch: relay.ResetEpoch})
		result.InstanceUID = cfg.DB.InstanceUID()
	default:
		return result, agentsViewUnavailable()
	}
	// Projection/route/key changes during I/O invalidate the selected result;
	// cursor progress alone does not. No fallback or transparent retarget occurs.
	current, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return agentsviewservice.Discovery{}, err
	}
	currentBinding, bindingErr := claimBinding(ctx, cfg.DB, projectID)
	currentPin, pinErr := cfg.DB.RootAuthority(ctx, project.UID)
	if current.UID != project.UID || bindingErr != nil || pinErr != nil || !reflect.DeepEqual(pin, currentPin) || !sameAgentsViewRoute(binding, currentBinding) {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	if _, err = agentsviewservice.DecodeDiscovery(raw, agentsViewPin(pin), path, time.Now()); err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	return result, nil
}

func sameAgentsViewRoute(a, b db.FederationBinding) bool {
	return a.Enabled == b.Enabled && a.Role == b.Role && a.HubURL == b.HubURL && a.HubProjectID == b.HubProjectID && a.HubProjectUID == b.HubProjectUID && reflect.DeepEqual(a.RelayConfig, b.RelayConfig)
}

func fetchAgentsViewService(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, credential config.FederationCredential, project string, pin db.RootKeyPin, path []string, relay db.RelayBindingConfig, peerInstanceUID string) (agentsviewservice.Discovery, error) {
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	client, err := agentsViewForwardHTTPClient(ctx, cfg, credential)
	if err != nil || client == nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	endpoint, err := httpurl.AppendHTTPBaseURLPath(credential.HubURL, fmt.Sprintf("/api/v1/projects/%d/federation/transcript-service", credential.HubProjectID))
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	query := url.Values{
		"project_uid":       {project},
		"authority_uid":     {pin.AuthorityUID},
		"binding_uid":       {relay.BindingUID},
		"peer_instance_uid": {peerInstanceUID},
		"protocol_version":  {fmt.Sprint(relay.ProtocolVersion)},
		"reset_epoch":       {fmt.Sprint(relay.ResetEpoch)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	req.Header.Set("Accept-Encoding", "identity")
	if err := addClaimForwardProjectScope(ctx, req); err != nil {
		return agentsviewservice.Discovery{}, err
	}
	resp, err := client.Do(req) //nolint:gosec // credential-pinned configured upstream only
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	encodings := resp.Header.Values("Content-Encoding")
	if resp.Uncompressed || len(encodings) > 1 || (len(encodings) == 1 && !strings.EqualFold(strings.TrimSpace(encodings[0]), "identity")) {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, agentsviewservice.MaxDiscoveryBytes+1))
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	result, err := agentsviewservice.DecodeDiscovery(raw, agentsViewPin(pin), path, time.Now())
	if err != nil {
		return agentsviewservice.Discovery{}, agentsViewUnavailable()
	}
	return result, nil
}

func agentsViewForwardHTTPClient(ctx context.Context, cfg ServerConfig, credential config.FederationCredential) (*http.Client, error) {
	// The synthetic Unix origin does not identify a socket. Resolving it via
	// the CLI runtime registry could send this enrollment to another daemon.
	if strings.HasPrefix(credential.HubURL, "http://kata.invalid") {
		return nil, errors.New("AgentsView upstream socket is not explicitly pinned")
	}
	if cfg.AgentsViewHubClient != nil {
		injected, err := cfg.AgentsViewHubClient(ctx, credential)
		if err != nil || injected == nil {
			return nil, errors.New("AgentsView upstream client unavailable")
		}
		transport := injected.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		raw, ok := transport.(*http.Transport)
		if !ok {
			return nil, errors.New("AgentsView upstream transport cannot enforce response bounds")
		}
		bounded := *injected
		clone := raw.Clone()
		clone.DisableCompression = true
		clone.ResponseHeaderTimeout = agentsViewHeaderTimeout
		clone.MaxResponseHeaderBytes = agentsViewMaxHeaderBytes
		bounded.Transport = clone
		bounded.Timeout = agentsViewRequestTimeout
		policy := config.BearerPolicy{
			TrustPrivateNetwork:    cfg.Auth.TrustPrivateNetwork,
			AllowInsecurePlaintext: credential.AllowInsecure,
		}
		if err := policy.ConfigureClient(&bounded, credential.HubURL, credential.Token); err != nil {
			return nil, err
		}
		if credential.Signing != nil {
			if err := federationsigning.ConfigureClient(&bounded, credential.HubURL, credential.Token, *credential.Signing); err != nil {
				return nil, err
			}
		}
		bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &bounded, nil
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("AgentsView upstream transport cannot enforce response bounds")
	}
	clone := transport.Clone()
	clone.DisableCompression = true
	clone.ResponseHeaderTimeout = agentsViewHeaderTimeout
	clone.MaxResponseHeaderBytes = agentsViewMaxHeaderBytes
	client := &http.Client{Transport: clone, Timeout: agentsViewRequestTimeout}
	policy := config.BearerPolicy{
		TrustPrivateNetwork:    cfg.Auth.TrustPrivateNetwork,
		AllowInsecurePlaintext: credential.AllowInsecure,
	}
	if err := policy.ConfigureClient(client, credential.HubURL, credential.Token); err != nil {
		return nil, err
	}
	if credential.Signing != nil {
		if err := federationsigning.ConfigureClient(client, credential.HubURL, credential.Token, *credential.Signing); err != nil {
			return nil, err
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, nil
}

func revalidateAgentsViewReader(ctx context.Context, project string) error {
	if err := authorizeProjectTarget(ctx, project); err != nil {
		return err
	}
	if decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision); decision != nil {
		if err := decision.revalidateRelease(ctx); err != nil {
			return err
		}
	}
	if state, ok := ctx.Value(hostAccessStateContextKey{}).(*hostAccessState); ok {
		// A callback is optional for short reads. Ask the controller again even
		// when it issued no long-lived lease callback.
		_, err := state.controller.Authorize(ctx, state.request)
		if errors.Is(err, ErrHostAccessDenied) {
			return projectAccessDenied()
		}
		if err != nil {
			return api.NewError(503, "access_unavailable", "access decision unavailable", "", nil)
		}
	}
	return nil
}
