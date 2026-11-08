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
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/transcriptservice"
)

func transcriptUnavailable() error {
	return api.NewError(http.StatusServiceUnavailable, "transcript_service_unavailable", "configured transcript service is unavailable for this project and route", "no transcript was read or uploaded", nil)
}

func transcriptPin(pin db.RootKeyPin) transcriptservice.TrustedRoot {
	return transcriptservice.TrustedRoot{ProjectUID: pin.ProjectUID, AuthorityUID: pin.AuthorityUID, KeyID: pin.KeyID, PublicKey: pin.PublicKey, Retired: pin.Retired}
}

// Discovery follows only the selected project's negotiated upstream. A root
// service entry on a relay never substitutes for unavailable upstream policy.
func discoverTranscriptService(ctx context.Context, cfg ServerConfig, projectID int64) (transcriptservice.Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	finish, err := beginClaimFederationTransport(ctx, cfg, projectID)
	if err != nil {
		return transcriptservice.Discovery{}, err
	}
	defer finish()
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return transcriptservice.Discovery{}, err
	}
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	pin, err := cfg.DB.RootAuthority(ctx, project.UID)
	if err != nil || db.ValidateRootKeyPin(pin) != nil || pin.Retired {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	var result transcriptservice.Discovery
	path := []string{cfg.DB.InstanceUID()}
	switch binding.Role {
	case db.FederationRoleHub:
		target := cfg.TranscriptService.Project[project.UID]
		if !target.Enabled || pin.AuthorityUID != cfg.DB.InstanceUID() || cfg.RootSigning == nil {
			return result, transcriptUnavailable()
		}
		signer := cfg.RootSigning.Signer()
		if !db.RootSignerMatches(pin, signer) {
			return result, transcriptUnavailable()
		}
		serviceID, policyErr := transcriptservice.ServiceID(target.ReceiverURL, target.LocatorURL)
		if policyErr != nil {
			return result, transcriptUnavailable()
		}
		now := time.Now().UTC()
		descriptor, signErr := transcriptservice.SignDescriptor(transcriptservice.Descriptor{
			Version: 1, ProjectUID: project.UID, AuthorityUID: pin.AuthorityUID, ServiceID: serviceID,
			LocatorBase: target.LocatorURL, EnvelopeVersion: 1, MaxBytes: transcriptservice.MaxSnapshotBytes,
			IssuedAt: now.Unix(), ExpiresAt: now.Add(transcriptservice.MaxLifetime).Unix(),
		}, signer.PrivateKey)
		if signErr != nil {
			return result, transcriptUnavailable()
		}
		result = transcriptservice.Discovery{Descriptor: descriptor, InstanceUID: cfg.DB.InstanceUID(), Route: []transcriptservice.RouteObservation{{InstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID}}}
	case db.FederationRoleSpoke:
		relay := binding.RelayConfig
		// Legacy cursor-only spokes have no negotiated route generation. Refuse
		// this new service rather than pretending they have the relay contract.
		if relay == nil || relay.UpstreamRevoked || relay.Validate(cfg.DB.InstanceUID()) != nil || relay.AuthorityUID != pin.AuthorityUID || binding.HubProjectUID != project.UID {
			return result, transcriptUnavailable()
		}
		path = append([]string(nil), relay.HubPath...)
		credential, _, credentialErr := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
		if credentialErr != nil || credential.LeavePending || credential.NativeLeave != nil || credential.RelayEnrollmentPending || credential.Token == "" {
			return result, transcriptUnavailable()
		}
		credentialURL, e1 := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
		bindingURL, e2 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
		if e1 != nil || e2 != nil || credentialURL != bindingURL || credential.HubProjectID != binding.HubProjectID {
			return result, transcriptUnavailable()
		}
		result, err = fetchTranscriptService(ctx, cfg, binding, credential, project.UID, pin, path[:len(path)-1])
		if err != nil {
			return transcriptservice.Discovery{}, err
		}
		result.Route = append(result.Route, transcriptservice.RouteObservation{InstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, BindingUID: relay.BindingUID, UpstreamInstanceUID: relay.UpstreamInstanceUID, ResetEpoch: relay.ResetEpoch})
		result.InstanceUID = cfg.DB.InstanceUID()
	default:
		return result, transcriptUnavailable()
	}
	// Projection/route/key changes during I/O invalidate the selected result;
	// cursor progress alone does not. No fallback or transparent retarget occurs.
	current, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return transcriptservice.Discovery{}, err
	}
	currentBinding, bindingErr := claimBinding(ctx, cfg.DB, projectID)
	currentPin, pinErr := cfg.DB.RootAuthority(ctx, project.UID)
	if current.UID != project.UID || bindingErr != nil || pinErr != nil || !reflect.DeepEqual(pin, currentPin) || !sameTranscriptRoute(binding, currentBinding) {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	if _, err = transcriptservice.DecodeDiscovery(raw, transcriptPin(pin), path, time.Now()); err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	return result, nil
}

func sameTranscriptRoute(a, b db.FederationBinding) bool {
	return a.Enabled == b.Enabled && a.Role == b.Role && a.HubURL == b.HubURL && a.HubProjectID == b.HubProjectID && a.HubProjectUID == b.HubProjectUID && reflect.DeepEqual(a.RelayConfig, b.RelayConfig)
}

func fetchTranscriptService(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, credential config.FederationCredential, project string, pin db.RootKeyPin, path []string) (transcriptservice.Discovery, error) {
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	var client *http.Client
	if cfg.FederationHubClient != nil {
		var err error
		client, err = cfg.FederationHubClient(ctx, credential)
		if err != nil || client == nil {
			return transcriptservice.Discovery{}, transcriptUnavailable()
		}
	} else {
		remote, _, err := newClaimForwardClient(ctx, binding, credential)
		if err != nil || remote.transportErr != nil {
			return transcriptservice.Discovery{}, transcriptUnavailable()
		}
		client = remote.client
	}
	bounded := *client
	bounded.Timeout = 10 * time.Second
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	endpoint, err := httpurl.AppendHTTPBaseURLPath(credential.HubURL, fmt.Sprintf("/api/v1/projects/%d/federation/transcript-service", credential.HubProjectID))
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	query := url.Values{"project_uid": {project}, "authority_uid": {pin.AuthorityUID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	if err := addClaimForwardProjectScope(ctx, req); err != nil {
		return transcriptservice.Discovery{}, err
	}
	resp, err := bounded.Do(req) //nolint:gosec // credential-pinned configured upstream only
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, transcriptservice.MaxDiscoveryBytes+1))
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	result, err := transcriptservice.DecodeDiscovery(raw, transcriptPin(pin), path, time.Now())
	if err != nil {
		return transcriptservice.Discovery{}, transcriptUnavailable()
	}
	return result, nil
}

func revalidateTranscriptReader(ctx context.Context, project string) error {
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
