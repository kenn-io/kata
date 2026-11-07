package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func cronRequestAPIError(ctx context.Context, cfg ServerConfig, err error, pid int64) error {
	switch {
	case errors.Is(err, db.ErrCronRequestExpired):
		return api.NewError(410, "request_expired", err.Error(), "Check clocks and generate a fresh request UID.", nil)
	case errors.Is(err, db.ErrCronRequestConflict):
		return api.NewError(409, "request_conflict", err.Error(), "Retry the original target, actor and TTL, or generate a fresh request UID.", nil)
	case errors.Is(err, db.ErrCronRequestUnavailable):
		return api.NewError(409, "request_unavailable", err.Error(), "Run requests require an enabled adapter execute job and an unpaused project; cancellation requires a running run.", nil)
	case errors.Is(err, db.ErrCronRequestCapacity):
		return api.NewError(409, "request_capacity", err.Error(), "Wait for retained requests to expire before submitting another request.", nil)
	case errors.Is(err, db.ErrFederatedSpokeUnsupported):
		return api.NewError(409, "federated_read_only", "cron requests belong to the owning hub", "Run the same command against the project's owning hub, for example with --daemon <hub-name>.", nil)
	default:
		return cronAPIError(ctx, cfg, err, pid, "", true)
	}
}

func admitCronRequest(ctx context.Context, cfg ServerConfig, pid int64, target, kind string, body api.CronRequestBody) (*api.CronRequestResponse, error) {
	ctx, actor, err := cronWriteContext(ctx, body.Actor)
	if err != nil {
		return nil, err
	}
	if _, err := activeProjectByID(ctx, cfg.DB, pid); err != nil {
		return nil, err
	}
	ttl := 0
	if body.TTLSeconds != nil {
		ttl = *body.TTLSeconds
	}
	result, err := cfg.DB.RequestCron(ctx, db.RequestCronIn{ProjectID: pid, UID: body.UID, TargetUID: strings.ToUpper(target), Kind: kind, Actor: actor, TTLSeconds: ttl})
	if err != nil {
		return nil, cronRequestAPIError(ctx, cfg, err, pid)
	}
	if result.Event != nil {
		cfg.Publish().Event(result.Event.ProjectID, *result.Event)
	}
	out := &api.CronRequestResponse{}
	out.Body.Request = api.CronRequest(result.Request)
	out.Body.Replayed = result.Replayed
	out.Body.Event = result.Event
	return out, nil
}

func registerCronRequestHandlers(h huma.API, cfg ServerConfig) {
	base := "/api/v1/projects/{project_id}/cron"
	huma.Register(h, huma.Operation{OperationID: "requestCronJobRun", Method: http.MethodPost, Path: base + "/jobs/{cron_uid}/requests"}, func(ctx context.Context, in *api.CronJobRequestRun) (*api.CronRequestResponse, error) {
		return admitCronRequest(ctx, cfg, in.ProjectID, in.UID, db.CronRequestKindRun, in.Body)
	})
	huma.Register(h, huma.Operation{OperationID: "requestCronRunCancel", Method: http.MethodPost, Path: base + "/runs/{run_uid}/cancel-request"}, func(ctx context.Context, in *api.CronRunRequestCancel) (*api.CronRequestResponse, error) {
		return admitCronRequest(ctx, cfg, in.ProjectID, in.RunUID, db.CronRequestKindCancel, in.Body)
	})
	huma.Register(h, huma.Operation{OperationID: "listCronRequests", Method: http.MethodGet, Path: base + "/requests"}, func(ctx context.Context, in *api.CronRequestListRequest) (*api.CronRequestListResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		requests, err := cfg.DB.ListCronRequests(ctx, db.CronRequestList{ProjectID: in.ProjectID, State: in.State})
		if err != nil {
			return nil, cronRequestAPIError(ctx, cfg, err, in.ProjectID)
		}
		out := &api.CronRequestListResponse{}
		out.Body.Requests = []api.CronRequest{}
		for _, request := range requests {
			out.Body.Requests = append(out.Body.Requests, api.CronRequest(request))
		}
		return out, nil
	})
}
