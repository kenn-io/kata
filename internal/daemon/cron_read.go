package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func cronJobResponses(ctx context.Context, cfg ServerConfig, projectID int64, jobs []db.CronJob, statuses []string) ([]api.CronJob, error) {
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		ids = append(ids, job.UID)
	}
	latest, err := cfg.DB.LatestCronRuns(ctx, projectID, ids)
	if err != nil {
		return nil, err
	}
	selected := map[string]bool{}
	for _, status := range statuses {
		selected[status] = true
	}
	result := []api.CronJob{}
	for _, job := range jobs {
		last, ran := latest[job.UID]
		status := "never_ran"
		if ran {
			status = last.Status
		}
		if len(selected) > 0 && !selected[status] {
			continue
		}
		value := api.CronJobFrom(job)
		if ran {
			lastResponse := api.CronRunFrom(last)
			value.LastRun = &lastResponse
		}
		result = append(result, value)
	}
	return result, nil
}

func registerCronSummaryHandler(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "getCronSummary", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/cron/summary"}, func(ctx context.Context, in *api.CronProjectRequest) (*api.CronSummaryResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		clock := cfg.UIClock
		if clock == nil {
			clock = time.Now
		}
		now := clock().UTC()
		since := now.Add(-24 * time.Hour)
		count, err := cfg.DB.CountFailedCronRuns(ctx, in.ProjectID, since, now)
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, "")
		}
		out := &api.CronSummaryResponse{}
		out.Body.FailedRuns24H = count
		out.Body.WindowStart = since
		out.Body.WindowEnd = now
		return out, nil
	})
}
