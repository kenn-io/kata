package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
	"go.kenn.io/kata/internal/uid"
)

type signedTrackerHopKey struct{}

func isTrackerRelayOperation(operation string) bool {
	return operation == "readTrackerRelay" || operation == "executeTrackerRelay"
}
func trackerControlProtocol(cfg ServerConfig) int {
	if cfg.TrackerRuntime == nil || cfg.HostAccess != nil || cfg.FederationSigning == nil {
		return 0
	}
	return tracker.ControlProtocolVersion
}
func trackerRelayPayloadDigest(p api.TrackerRelayPayload, mutation bool) (string, error) {
	if !uid.Valid(p.UID) {
		return "", tracker.ErrInvalid
	}
	switch p.Kind {
	case "execute":
		if !mutation || p.Operation == nil || p.Operation.UID != p.UID {
			return "", tracker.ErrInvalid
		}
		if _, err := trackerOperationDigest(*p.Operation); err != nil {
			return "", err
		}
	case "discover", "receipt":
		if mutation || p.Operation != nil {
			return "", tracker.ErrInvalid
		}
	default:
		return "", tracker.ErrInvalid
	}
	raw, err := json.Marshal(p, json.Deterministic(true))
	if err != nil || len(raw) > 48<<10 {
		return "", tracker.ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func registerTrackerRelayHandlers(humaAPI huma.API, cfg ServerConfig) {
	for _, mutation := range []bool{false, true} {
		operation, path, capability := "readTrackerRelay", "/api/v1/projects/{project_id}/federation/tracker/read", "pull"
		if mutation {
			operation, path, capability = "executeTrackerRelay", "/api/v1/projects/{project_id}/federation/tracker/control", "push"
		}
		huma.Register(humaAPI, huma.Operation{OperationID: operation, Method: http.MethodPost, Path: path}, func(ctx context.Context, in *api.TrackerRelayRequest) (*api.TrackerRelayResponse, error) {
			if verified, _ := ctx.Value(signedTrackerHopKey{}).(bool); !verified || trackerControlProtocol(cfg) != tracker.ControlProtocolVersion {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			ctx, peer, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, capability, federationTransportOperation(operation))
			if err != nil {
				return nil, err
			}
			if peer.ScopedProjectID != in.ProjectID || peer.RelayProtocolVersion != db.RelayProtocolVersion {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			if needsReset, err := relayEnrollmentNeedsReset(ctx, cfg.DB, peer.RelayBindingUID); err != nil || needsReset {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
			if err != nil {
				return nil, trackerAPIError(err)
			}
			a := in.Body.Assertion
			if a.ProjectUID != project.UID {
				return nil, trackerAPIError(tracker.ErrDenied)
			}
			if err := a.Validate(cfg.DB.InstanceUID(), peer.SpokeInstanceUID, peer.RelayBindingUID, peer.RelayResetEpoch, peer.RelayServeDownstream, time.Now()); err != nil {
				return nil, trackerAPIError(err)
			}
			ctx, cancel := context.WithDeadline(ctx, time.Unix(a.ExpiresAt, 0))
			defer cancel()
			digest, err := trackerRelayPayloadDigest(in.Body.Payload, mutation)
			if err != nil {
				return nil, trackerAPIError(err)
			}
			if digest != a.RequestDigest || in.Body.Payload.UID != a.OperationUID {
				return nil, trackerAPIError(tracker.ErrInvalid)
			}
			binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
			if err != nil {
				return nil, trackerAPIError(trackerRelayStepError(ctx, err, errTrackerRelayUnavailable))
			}
			if !binding.Enabled {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			if binding.Role == db.FederationRoleSpoke {
				if binding.RelayConfig == nil || !binding.RelayConfig.ServeDownstream || binding.RelayConfig.AuthorityUID != a.RootInstanceUID {
					return nil, trackerAPIError(errTrackerRelayUnavailable)
				}
				result, err := forwardTrackerRelay(ctx, cfg, project, in.Body)
				if err != nil {
					return nil, trackerAPIError(err)
				}
				return &api.TrackerRelayResponse{Body: result}, nil
			}
			if binding.Role != db.FederationRoleHub || a.RootInstanceUID != cfg.DB.InstanceUID() {
				return nil, trackerAPIError(tracker.ErrDenied)
			}
			result, err := trackerRootRequest(ctx, cfg, project, a.Source, in.Body.Payload)
			if err != nil {
				return nil, trackerAPIError(err)
			}
			return &api.TrackerRelayResponse{Body: result}, nil
		})
	}
}

func trackerRootRequest(ctx context.Context, cfg ServerConfig, project db.Project, p tracker.Principal, payload api.TrackerRelayPayload) (api.TrackerRelayResult, error) {
	result := api.TrackerRelayResult{RootInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID}
	switch payload.Kind {
	case "discover":
		connections, err := cfg.TrackerRuntime.Connections(ctx, project.UID, p)
		if err != nil {
			return result, err
		}
		result.Connections = &api.TrackerConnectionsBody{RootInstanceUID: result.RootInstanceUID, ProjectUID: project.UID, Connections: connections}
	case "execute":
		if payload.Operation == nil {
			return result, tracker.ErrInvalid
		}
		out, err := runTrackerOperation(ctx, cfg, project, p, *payload.Operation)
		if err != nil {
			return result, err
		}
		result.Operation = &out
	case "receipt":
		snapshot, err := cfg.TrackerRuntime.current()
		if err != nil {
			return result, err
		}
		ctx, err = cfg.TrackerRuntime.mappedContext(ctx, snapshot, project.UID, p)
		if err != nil {
			return result, err
		}
		controls, ok := cfg.DB.(db.TrackerControlStore)
		if !ok {
			return result, tracker.ErrUnavailable
		}
		receipt, err := controls.ReadTrackerControl(ctx, project.UID, payload.UID)
		if err != nil {
			return result, err
		}
		if receipt.Principal != p {
			return result, db.ErrNotFound
		}
		out := trackerOperationOut(receipt)
		result.Operation = &out
	default:
		return result, tracker.ErrInvalid
	}
	return result, nil
}

func trackerProjectRequest(ctx context.Context, cfg ServerConfig, project db.Project, p tracker.Principal, payload api.TrackerRelayPayload) (api.TrackerRelayResult, error) {
	if _, err := trackerRelayPayloadDigest(payload, payload.Kind == "execute"); err != nil {
		return api.TrackerRelayResult{}, err
	}
	binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return api.TrackerRelayResult{}, err
	}
	if err == nil && binding.Role == db.FederationRoleSpoke {
		// Numeric IDs and credentials are translated only by the pinned hop helper.
		// A disabled replica never falls back to local provider configuration.
		if !binding.Enabled || binding.RelayConfig == nil {
			return api.TrackerRelayResult{}, errTrackerRelayUnavailable
		}
		digest, _ := trackerRelayPayloadDigest(payload, payload.Kind == "execute")
		a := tracker.RelayAssertion{Version: tracker.ControlProtocolVersion, RootInstanceUID: binding.RelayConfig.AuthorityUID, ProjectUID: project.UID, Source: p, OperationUID: payload.UID, RequestDigest: digest, ExpiresAt: time.Now().Add(30 * time.Second).Unix()}
		return forwardTrackerRelay(ctx, cfg, project, api.TrackerRelayBody{Assertion: a, Payload: payload})
	}
	return trackerRootRequest(ctx, cfg, project, p, payload)
}
