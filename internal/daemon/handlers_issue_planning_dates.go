package daemon

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerIssuePlanningDateHandlers(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "issuePlanningDates", Method: "GET", Path: "/api/v1/projects/{project_id}/issues/{ref}/planning-dates"}, func(ctx context.Context, in *api.ShowIssueRequest) (*api.IssuePlanningDatesResponse, error) {
		include := db.IncludeDeletedNo
		if in.IncludeDeleted {
			include = db.IncludeDeletedYes
		}
		issue, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, include)
		if err != nil {
			return nil, err
		}
		dates, err := cfg.DB.IssuePlanningDates(ctx, db.IssuePlanningDatesIn{ProjectID: in.ProjectID, IssueID: issue.ID, IncludeDeleted: in.IncludeDeleted, DefaultTimezone: cfg.DefaultTimezone})
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, api.NewError(404, "issue_not_found", "issue not found", "", nil)
			}
			if errors.Is(err, db.ErrPlanningDatesInvalid) {
				return nil, api.NewError(422, "planning_dates_invalid", err.Error(), "correct the issue planning-date metadata", nil)
			}
			return nil, internalAPIError(err)
		}
		return &api.IssuePlanningDatesResponse{Body: api.IssuePlanningDates(dates)}, nil
	})
}
