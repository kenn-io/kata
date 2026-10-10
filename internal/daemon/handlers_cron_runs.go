package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func cronRunAPIError(ctx context.Context, cfg ServerConfig, err error, projectID int64, id string) error {
	if errors.Is(err, db.ErrCronConflict) {
		data := map[string]any{}
		if current, readErr := cfg.DB.CronRun(ctx, projectID, id); readErr == nil {
			data["current"] = api.CronRunFrom(current)
		}
		return api.NewError(409, "cron_conflict", err.Error(), "refresh the run observation before retrying", data)
	}
	return cronAPIError(ctx, cfg, err, projectID, "", true)
}
func registerCronRunHandlers(h huma.API, cfg ServerConfig) {
	base := "/api/v1/projects/{project_id}/cron"
	huma.Register(h, huma.Operation{OperationID: "getCronCapabilities", Method: http.MethodGet, Path: base + "/capabilities"}, func(ctx context.Context, in *api.CronProjectRequest) (*api.CronCapabilitiesResponse, error) {
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		out := &api.CronCapabilitiesResponse{EventFeatures: db.CronEventFeature}
		out.Body.ProjectUID = project.UID
		out.Body.EventFeatures = []string{db.CronEventFeature}
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "observeCronRun", Method: http.MethodPut, Path: base + "/runs/{run_uid}", MaxBodyBytes: db.CronObservationLimit}, func(ctx context.Context, in *api.ObserveCronRunRequest) (*api.ObserveCronRunResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		result, err := cfg.DB.ObserveCronRun(ctx, in.Body.Native(in.ProjectID, in.RunUID, actor))
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, in.RunUID)
		}
		cfg.Publish().Events(in.ProjectID, result.Events)
		out := &api.ObserveCronRunResponse{}
		out.Body.Run = api.CronRunFrom(result.Run)
		out.Body.Events = result.Events
		out.Body.Replayed = result.Replayed
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "showCronRun", Method: http.MethodGet, Path: base + "/runs/{run_uid}"}, func(ctx context.Context, in *api.CronRunRequest) (*api.CronRunResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		run, err := cfg.DB.CronRun(ctx, in.ProjectID, in.RunUID)
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, in.RunUID)
		}
		out := &api.CronRunResponse{}
		out.Body.Run = api.CronRunFrom(run)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronRuns", Method: http.MethodGet, Path: base + "/runs"}, func(ctx context.Context, in *api.CronRunsRequest) (*api.CronRunsResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		runs, err := cfg.DB.ListCronRuns(ctx, db.CronRunList{ProjectID: in.ProjectID, JobUID: in.JobUID, Limit: in.Limit, BeforeUID: in.BeforeUID})
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, "")
		}
		out := &api.CronRunsResponse{}
		out.Body.Runs = []api.CronRun{}
		for _, run := range runs {
			out.Body.Runs = append(out.Body.Runs, api.CronRunFrom(run))
		}
		limit := in.Limit
		if limit == 0 || limit > 100 {
			limit = 100
		}
		if len(runs) == limit {
			out.Body.NextBeforeUID = runs[len(runs)-1].UID
		}
		return out, nil
	})
}
