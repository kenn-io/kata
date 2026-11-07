package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronRunCountsHandler(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "getCronRunCounts", Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/cron/runs/counts"}, func(ctx context.Context, in *api.CronRunCountsRequest) (*api.CronRunCountsResponse, error) {
		// Storage pins authorization, metadata, retained rows and names to one
		// snapshot. A separate definition lookup could mix two different states.
		result, err := cfg.DB.CronRunCounts(ctx, db.CronRunCountsQuery{ProjectID: in.ProjectID, JobUID: in.JobUID, WorkflowUID: in.WorkflowUID})
		if err != nil {
			return nil, cronRunAPIError(ctx, cfg, err, in.ProjectID, "")
		}
		return &api.CronRunCountsResponse{Body: result}, nil
	})
}
