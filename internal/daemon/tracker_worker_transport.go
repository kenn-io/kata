package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/tracker"
)

type trackerWorkerBody struct {
	Signal   *trackerWorkerSignal       `json:"signal,omitempty"`
	Presence tracker.WorkerPresence     `json:"presence"`
	Reply    *trackerWorkerReply        `json:"reply,omitempty"`
	Chunk    *tracker.WorkerResultChunk `json:"chunk,omitempty"`
}
type trackerWorkerRequest struct {
	ProjectID     int64  `path:"project_id"`
	Authorization string `header:"Authorization"`
	Body          trackerWorkerBody
}
type trackerWorkerResponseBody struct {
	Decision        *trackerWorkerDecision `json:"decision,omitempty"`
	RootInstanceUID string                 `json:"root_instance_uid"`
	ProjectUID      string                 `json:"project_uid"`
	Commands        []trackerWorkerCommand `json:"commands"`
}
type trackerWorkerResponse struct{ Body trackerWorkerResponseBody }

var trackerWorkerRoutes = []struct {
	id, path, capability string
	mutation             bool
}{
	{"pollTrackerWorker", "poll", "pull", false},
	{"replyTrackerWorker", "reply", "push", true},
	{"uploadTrackerWorkerChunk", "chunk", "push", true},
	{"admitTrackerWorker", "admit", "push", true},
	{"settleTrackerWorkerMetadata", "metadata", "push", true},
}

func isTrackerWorkerOperation(operation string) bool {
	for _, route := range trackerWorkerRoutes {
		if route.id == operation {
			return true
		}
	}
	return false
}
func registerTrackerWorkerHandlers(humaAPI huma.API, cfg ServerConfig) {
	for _, route := range trackerWorkerRoutes {
		huma.Register(humaAPI, huma.Operation{OperationID: route.id, Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/federation/tracker/worker/" + route.path, MaxBodyBytes: 1 << 20}, func(ctx context.Context, in *trackerWorkerRequest) (*trackerWorkerResponse, error) {
			if verified, _ := ctx.Value(signedTrackerHopKey{}).(bool); !verified || trackerWorkerProtocol(cfg) == 0 {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			ctx, peer, err := authorizeFederationRequest(ctx, cfg, in.Authorization, in.ProjectID, route.capability, federationTransportOperation(route.id))
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
			p := in.Body.Presence
			if p.ProjectUID != project.UID {
				return nil, trackerAPIError(tracker.ErrDenied)
			}
			if err := p.Validate(cfg.DB.InstanceUID(), peer.SpokeInstanceUID, peer.RelayBindingUID, peer.RelayResetEpoch, peer.RelayServeDownstream, time.Now()); err != nil {
				return nil, trackerAPIError(err)
			}
			if (route.path == "poll" && (in.Body.Reply != nil || in.Body.Chunk != nil || in.Body.Signal != nil)) || (route.path == "reply" && (in.Body.Reply == nil || in.Body.Chunk != nil || in.Body.Signal != nil)) || (route.path == "chunk" && (in.Body.Chunk == nil || in.Body.Reply != nil || in.Body.Signal != nil)) {
				return nil, trackerAPIError(tracker.ErrInvalid)
			}
			binding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
			if err != nil || !binding.Enabled {
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			if binding.Role == db.FederationRoleSpoke {
				if binding.RelayConfig == nil || !binding.RelayConfig.ServeDownstream || binding.RelayConfig.AuthorityUID != p.RootInstanceUID {
					return nil, trackerAPIError(errTrackerRelayUnavailable)
				}
				result, err := forwardTrackerWorker(ctx, cfg, project, in.Body, route.path)
				if err != nil {
					return nil, trackerAPIError(err)
				}
				return &trackerWorkerResponse{Body: result}, nil
			}
			if binding.Role != db.FederationRoleHub || p.RootInstanceUID != cfg.DB.InstanceUID() {
				return nil, trackerAPIError(tracker.ErrDenied)
			}
			result := trackerWorkerResponseBody{RootInstanceUID: cfg.DB.InstanceUID(), ProjectUID: project.UID, Commands: []trackerWorkerCommand{}}
			switch route.path {
			case "poll":
				if err := cfg.TrackerRuntime.workerBroker.remember(p, time.Now()); err != nil {
					return nil, trackerAPIError(err)
				}
				result.Commands = cfg.TrackerRuntime.workerBroker.poll(project.UID, p.OwnerInstanceUID, time.Now())
			case "reply":
				if err := cfg.TrackerRuntime.workerBroker.reply(project.UID, p.OwnerInstanceUID, *in.Body.Reply); err != nil {
					return nil, trackerAPIError(err)
				}
			case "admit":
				if in.Body.Signal == nil || in.Body.Reply != nil || in.Body.Chunk != nil {
					return nil, trackerAPIError(tracker.ErrInvalid)
				}
				decision, err := cfg.TrackerRuntime.workerBroker.writeSignal(ctx, project.UID, p.OwnerInstanceUID, p.OwnerEpoch, *in.Body.Signal)
				if err != nil {
					return nil, trackerAPIError(err)
				}
				result.Decision = &decision
			case "metadata":
				if in.Body.Signal == nil || in.Body.Chunk != nil || in.Body.Reply != nil {
					return nil, trackerAPIError(tracker.ErrInvalid)
				}
				if err := acceptWorkerTerminal(ctx, cfg, project.UID, p.OwnerInstanceUID, *in.Body.Signal); err != nil {
					return nil, trackerAPIError(err)
				}
			case "chunk":
				if err := cfg.TrackerRuntime.workerBroker.chunk(project.UID, p.OwnerInstanceUID, *in.Body.Chunk); err != nil {
					return nil, trackerAPIError(err)
				}
			default:
				return nil, trackerAPIError(errTrackerRelayUnavailable)
			}
			return &trackerWorkerResponse{Body: result}, nil
		})
	}
}

func trackerWorkerProtocol(cfg ServerConfig) int {
	if trackerControlProtocol(cfg) == 0 {
		return 0
	}
	return tracker.WorkerProtocolVersion
}
