package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronReapHandler(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "reapCronRun", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/runs/{run_uid}/reap", MaxBodyBytes: 8192}, func(ctx context.Context, in *api.ReapCronRunRequest) (*api.ReapCronRunResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		result, err := cfg.DB.ReapCronRun(ctx, db.ReapCronRun{ProjectID: in.ProjectID, UID: in.RunUID, Actor: actor, ExpectedRevision: in.Body.ExpectedRevision, Message: in.Body.Message, Force: in.Body.Force, Now: cronNow(cfg)})
		if errors.Is(err, db.ErrCronRunNotRunning) {
			return nil, api.NewError(409, "cron_run_not_running", err.Error(), "refresh the run before retrying", nil)
		}
		if staleErr, ok := errors.AsType[*db.CronRunNotStaleError](err); ok {
			return nil, api.NewError(409, "cron_run_not_stale", err.Error(), "wait for the deadline or explicitly force reap", map[string]any{"stale_at": staleErr.StaleAt})
		}
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, in.RunUID)
		}
		cfg.Publish().Events(in.ProjectID, result.Events)
		out := &api.ReapCronRunResponse{}
		out.Body.Run, out.Body.Events = api.CronRunFrom(result.Run), result.Events
		return out, nil
	})
}
