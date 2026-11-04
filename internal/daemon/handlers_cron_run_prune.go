package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronRunPruneHandler(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "pruneCronRuns", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/runs/actions/prune"}, func(ctx context.Context, in *api.PruneCronRunsRequest) (*api.PruneCronRunsResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, in.Body.Actor)
		if err != nil {
			return nil, err
		}
		project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if in.Body.Before.IsZero() || in.Body.Before.After(time.Now().Add(-24*time.Hour)) {
			return nil, api.NewError(400, "validation", "before must be at least 24 hours old", "", nil)
		}
		if !in.Body.DryRun {
			if err := validateExactConfirm(in.Confirm, fmt.Sprintf("PRUNE %s cron runs", project.Name)); err != nil {
				return nil, err
			}
		}
		result, err := cfg.DB.PruneCronRuns(ctx, db.PruneCronRuns{ProjectID: project.ID, Actor: actor, Before: in.Body.Before, Keep: in.Body.Keep, Limit: in.Body.Limit, JobUID: in.Body.JobUID, WorkflowUID: in.Body.WorkflowUID, DryRun: in.Body.DryRun})
		if errors.Is(err, db.ErrFederatedSpokeUnsupported) {
			return nil, api.NewError(409, "federated_admin_required", err.Error(), "run this command against the project's owning hub", nil)
		}
		if errors.Is(err, db.ErrFederationResetBlockedByPendingPush) {
			return nil, api.NewError(409, "federation_pending_delivery", "relay delivery is still pending", "sync retained deliveries or explicitly revoke their enrollments before pruning", nil)
		}
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, project.ID, "", true)
		}
		if result.MetadataEvent.ID > 0 {
			cfg.Publish().Event(project.ID, result.MetadataEvent)
		}
		if result.ResetAfterEventID > 0 {
			cfg.Publish().Reset(project.ID, result.ResetAfterEventID)
			if err := refreshFederationBaselineAfterReset(ctx, cfg, project.ID, actor); err != nil {
				return nil, internalAPIError(err)
			}
		}
		return &api.PruneCronRunsResponse{Body: result}, nil
	})
}
