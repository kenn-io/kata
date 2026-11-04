package daemon

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func cronFleetScope(ctx context.Context, ids []int64) (context.Context, []int64, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	ctx, err := authorizeHostProjectScope(ctx, ids, nil, len(ids) == 0)
	return ctx, ids, err
}

func cronFleetProjects(ctx context.Context, store db.Storage, ids []int64) ([]api.CronFleetProject, error) {
	slices.Sort(ids)
	ids = slices.Compact(ids)
	out := make([]api.CronFleetProject, 0, len(ids))
	for _, id := range ids {
		p, err := store.ProjectByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, api.CronFleetProject{ID: p.ID, UID: p.UID, Name: p.Name})
	}
	return out, nil
}

func registerCronFleetHandlers(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "listAllCronRuns", Method: http.MethodGet, Path: "/api/v1/cron/runs"}, func(ctx context.Context, in *api.AllCronRunsRequest) (*api.AllCronRunsResponse, error) {
		ctx, ids, err := cronFleetScope(ctx, in.ProjectIDs)
		if err != nil {
			return nil, err
		}
		query, err := in.Native(0, ids, true)
		if err != nil {
			return nil, err
		}
		runs, err := cfg.DB.ListCronRuns(ctx, query)
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, 0, "")
		}
		out := &api.AllCronRunsResponse{}
		out.Body.Runs = []api.CronRun{}
		projectIDs := []int64{}
		for _, run := range runs {
			out.Body.Runs = append(out.Body.Runs, api.CronRunFrom(run))
			projectIDs = append(projectIDs, run.ProjectID)
		}
		if len(runs) == query.Limit {
			out.Body.NextBeforeUID = runs[len(runs)-1].UID
		}
		filters := query.Filters()
		out.Body.Filters = &filters
		out.Body.Projects, err = cronFleetProjects(ctx, cfg.DB, projectIDs)
		if err != nil {
			return nil, internalAPIError(err)
		}
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listAllCronJobs", Method: http.MethodGet, Path: "/api/v1/cron/jobs"}, func(ctx context.Context, in *api.AllCronJobsRequest) (*api.AllCronJobsResponse, error) {
		ctx, ids, err := cronFleetScope(ctx, in.ProjectIDs)
		if err != nil {
			return nil, err
		}
		jobs, err := cfg.DB.ListAllCronJobs(ctx, db.CronAllList{ProjectIDs: ids, IncludeDeleted: in.IncludeDeleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, 0, "", true)
		}
		out := &api.AllCronJobsResponse{}
		out.Body.Jobs = []api.CronJob{}
		projectIDs := []int64{}
		for _, job := range jobs {
			out.Body.Jobs = append(out.Body.Jobs, api.CronJobFrom(job))
			projectIDs = append(projectIDs, job.ProjectID)
		}
		out.Body.Projects, err = cronFleetProjects(ctx, cfg.DB, projectIDs)
		if err != nil {
			return nil, internalAPIError(err)
		}
		return out, nil
	})
}
