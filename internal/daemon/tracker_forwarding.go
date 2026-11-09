package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/tracker"
)

// forwardTrackerRelay uses the bound upstream transport only. It never sends
// the local daemon bearer, provider credentials or the user's API token. Every
// hop translates the numeric ID after checking the portable project identity.
func forwardTrackerRelay(ctx context.Context, cfg ServerConfig, project db.Project, body api.TrackerRelayBody) (api.TrackerRelayResult, error) {
	empty := api.TrackerRelayResult{}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(body.Assertion.ExpiresAt, 0))
	defer cancel()
	release, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), project.ID), cfg.DB, project.ID)
	if err != nil {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	defer release()
	binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil || binding.Role != db.FederationRoleSpoke || !binding.Enabled || binding.RelayConfig == nil {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	route := binding.RelayConfig
	if route.Validate(cfg.DB.InstanceUID()) != nil || route.UpstreamRevoked || route.AuthorityUID != body.Assertion.RootInstanceUID || project.UID != body.Assertion.ProjectUID || len(body.Assertion.Path) >= tracker.MaxControlHops || slices.ContainsFunc(body.Assertion.Path, func(h tracker.RelayHop) bool { return h.InstanceUID == cfg.DB.InstanceUID() }) {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	if body.Payload.Kind == "execute" && !binding.PushEnabled {
		return empty, trackerRelayStepError(ctx, err, tracker.ErrDenied)
	}
	credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || credential.Token == "" || credential.Signing == nil || credential.RelayEnrollmentPending || credential.LeavePending || cfg.FederationHubClient == nil {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	origin, e1 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	credentialOrigin, e2 := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	if e1 != nil || e2 != nil || origin != credentialOrigin || credential.HubProjectID != binding.HubProjectID {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	credential = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, credential)
	client, err := cfg.FederationHubClient(ctx, credential)
	if err != nil || client == nil {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	// Copy rather than changing a shared transport client's redirect policy.
	selectedClient := *client
	selectedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base := fmt.Sprintf("%s/api/v1/projects/%d", origin, binding.HubProjectID)
	var meta api.ProjectFederationBody
	if err := trackerHubRequest(ctx, &selectedClient, http.MethodGet, base+"/federation/metadata", nil, &meta); err != nil {
		return empty, err
	}
	pin, err := cfg.DB.RootAuthority(ctx, project.UID)
	if err != nil || meta.TrackerControlVersion != tracker.ControlProtocolVersion || meta.ProjectUID != project.UID || meta.ProjectID != binding.HubProjectID || meta.Relay == nil || meta.Relay.ProtocolVersion != db.RelayProtocolVersion || meta.Relay.UpstreamInstanceUID != route.UpstreamInstanceUID || meta.Relay.BindingUID != route.BindingUID || meta.Relay.ResetEpoch != route.ResetEpoch || meta.Relay.ResetRequired || !slices.Equal(meta.Relay.HubPath, route.HubPath) || meta.Relay.Root.AuthorityUID != route.AuthorityUID || meta.Relay.Root.ProjectUID != project.UID || meta.Relay.Root.KeyID != pin.KeyID || !bytes.Equal(meta.Relay.Root.PublicKey, pin.PublicKey) {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	// A rejoin can bypass the sync gate. Recheck its exact route and live local
	// project/token authority after negotiation, before submitting any control.
	current, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil || current.Role != binding.Role || !current.Enabled || current.PushEnabled != binding.PushEnabled || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure || !reflect.DeepEqual(current.RelayConfig, route) {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	if err := cronForwardAdmission(ctx, cfg.DB, project.ID, project.UID); err != nil {
		return empty, trackerRelayStepError(ctx, err, tracker.ErrDenied)
	}
	assertion := body.Assertion
	assertion.Path = append(slices.Clone(assertion.Path), tracker.RelayHop{InstanceUID: cfg.DB.InstanceUID(), BindingUID: route.BindingUID, ResetEpoch: route.ResetEpoch})
	assertion.IssuerInstanceUID = cfg.DB.InstanceUID()
	assertion.AudienceInstanceUID = route.UpstreamInstanceUID
	body.Assertion = assertion
	suffix := "/federation/tracker/read"
	if body.Payload.Kind == "execute" {
		suffix = "/federation/tracker/control"
	}
	var result api.TrackerRelayResult
	if err := trackerHubRequest(ctx, &selectedClient, http.MethodPost, base+suffix, body, &result); err != nil {
		return empty, err
	}
	if result.RootInstanceUID != route.AuthorityUID || result.ProjectUID != project.UID {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	if body.Payload.Kind == "discover" {
		if result.Connections == nil || result.Operation != nil || result.Connections.RootInstanceUID != result.RootInstanceUID || result.Connections.ProjectUID != project.UID {
			return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
		}
	} else if result.Operation == nil || result.Connections != nil || result.Operation.UID != body.Payload.UID || result.Operation.RootInstanceUID != result.RootInstanceUID || result.Operation.ProjectUID != project.UID {
		return empty, trackerRelayStepError(ctx, err, errTrackerRelayUnavailable)
	}
	// Imported events arrive through ordinary federation; never apply response
	// projection IDs or provider operations at the replica.
	return result, nil
}

func trackerHubRequest(ctx context.Context, client *http.Client, method, path string, body, out any) error {
	return trackerHubRequestBounded(ctx, client, method, path, body, out, 64<<10)
}
func trackerHubRequestBounded(ctx context.Context, client *http.Client, method, path string, body, out any, limit int) error {
	if limit != 64<<10 && limit != 1<<20 {
		return tracker.ErrInvalid
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body, json.Deterministic(true))
		if err != nil || len(payload) > limit {
			return tracker.ErrInvalid
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(payload))
	if err != nil {
		return errTrackerRelayUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	if err := addClaimForwardProjectScope(ctx, request); err != nil {
		return tracker.ErrDenied
	}
	response, err := client.Do(request) //nolint:gosec // G704: exact configured federation origin with its scoped, origin-pinned client; redirects rejected.
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errTrackerRelayUnavailable
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errTrackerRelayUnavailable
	}
	if response.StatusCode != http.StatusOK {
		// Never forward arbitrary upstream messages, bodies or headers. Preserve
		// only known tracker classes; an old/mixed peer has no fallback endpoint.
		var envelope api.ErrorEnvelope
		if json.Unmarshal(raw, &envelope) == nil {
			switch envelope.Error.Code {
			case "tracker_operation_unsupported":
				return errTrackerOperationUnsupported
			case "tracker_binding_disabled":
				return db.ErrIssueSyncNotEnabled
			case "tracker_running":
				return db.ErrIssueSyncAlreadyRunning
			case "tracker_project_already_bound":
				return db.ErrIssueSyncProjectAlreadyBound
			case "tracker_denied":
				return tracker.ErrDenied
			case "tracker_operation_conflict":
				return tracker.ErrControlConflict
			case "tracker_operation_pending":
				return tracker.ErrControlPending
			case "tracker_capacity":
				return tracker.ErrControlCapacity
			case "tracker_selection_required":
				return tracker.ErrSelectionRequired
			case "tracker_outcome_unknown":
				return db.ErrTrackerOutcomeUnknown
			case "tracker_timeout":
				return context.DeadlineExceeded
			case "tracker_activation_required":
				return db.ErrTrackerActivationRequired
			case "tracker_binding_changed":
				return db.ErrIssueSyncBindingChanged
			case "not_found":
				return db.ErrNotFound
			case "validation":
				return tracker.ErrInvalid
			}
		}
		return errTrackerRelayUnavailable
	}
	if json.Unmarshal(raw, out) != nil {
		return errTrackerRelayUnavailable
	}
	return nil
}

// Preserve only a deadline class at pre-submission I/O boundaries. Every
// refusal remains fail-closed; a deadline never renews forwarding authority.
func trackerRelayStepError(ctx context.Context, err, fallback error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}
