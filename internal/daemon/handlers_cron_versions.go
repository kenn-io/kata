package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronVersionHandlers(h huma.API, cfg ServerConfig) {
	for _, kind := range []struct {
		path, operation string
		job             bool
	}{{"jobs", "listCronJobVersions", true}, {"workflows", "listCronWorkflowVersions", false}} {
		huma.Register(h, huma.Operation{OperationID: kind.operation, Method: http.MethodGet, Path: "/api/v1/projects/{project_id}/cron/" + kind.path + "/{cron_uid}/versions"}, func(ctx context.Context, in *api.ListCronVersionsRequest) (*api.ListCronVersionsResponse, error) {
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			list := cfg.DB.ListCronWorkflowVersions
			if kind.job {
				list = cfg.DB.ListCronJobVersions
			}
			page, err := list(ctx, db.CronVersionList{ProjectID: in.ProjectID, UID: in.UID, Limit: in.Limit, BeforeEventUID: in.BeforeEventUID})
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, kind.job)
			}
			return api.CronVersionsFrom(page), nil
		})
	}
}
