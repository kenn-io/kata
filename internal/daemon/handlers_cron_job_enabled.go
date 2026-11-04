package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func registerCronJobEnabledHandlers(h huma.API, cfg ServerConfig) {
	handle := func(ctx context.Context, projectID int64, id, authorization string, body api.CronJobEnabledActionBody, enabled, discard bool) (*api.CronJobResponse, error) {
		action := "pause"
		if enabled {
			action = "resume"
		}
		ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, projectID, authorization, body.Actor, action+"CronJob")
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
			return nil, err
		}
		current, err := cfg.DB.CronJob(ctx, projectID, id)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, true)
		}
		if enrollment && !cron.IsRecurringIssueJob(current.Definition) {
			return nil, federationCredentialDenied()
		}
		if current.DeletedAt != nil || (body.ExpectedEventUID != "" && body.ExpectedEventUID != current.DefinitionEventUID) {
			return nil, cronAPIError(ctx, cfg, db.ErrCronConflict, projectID, id, true)
		}
		if discard {
			result, err := cfg.DB.ResumeCronJob(ctx, db.ResumeCronJobIn{ProjectID: projectID, UID: id, Actor: actor, ExpectedEventUID: body.ExpectedEventUID, DiscardMissed: true})
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, projectID, id, true)
			}
			cfg.Publish().Events(projectID, result.Events)
			return cronJobControlResponse(ctx, cfg, projectID, result.Job.UID, result.Events)
		}
		definition := current.Definition
		definition.Enabled = enabled
		write := api.PutCronJobBody{Actor: actor, Name: current.Name, Definition: api.CronJobDefinitionFrom(definition), ExpectedEventUID: current.DefinitionEventUID}
		// Local replicas cannot prove a hub no-op. A legacy conditional PUT keeps
		// old hubs usable without a job GET that enrollment credentials cannot make.
		if out, handled, err := forwardCronJob(ctx, cfg, projectID, id, http.MethodPut, "", &write, nil, enrollment); handled {
			return out, err
		}
		if current.Definition.Enabled == enabled {
			return cronJobControlResponse(ctx, cfg, projectID, current.UID, nil)
		}
		value, events, err := cfg.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: projectID, UID: id, ExpectedEventUID: current.DefinitionEventUID, Name: current.Name, Definition: definition, Actor: actor})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, true)
		}
		cfg.Publish().Events(projectID, events)
		return cronJobControlResponse(ctx, cfg, projectID, value.UID, events)
	}
	huma.Register(h, huma.Operation{OperationID: "pauseCronJob", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}/pause"}, func(ctx context.Context, in *api.CronJobEnabledActionRequest) (*api.CronJobResponse, error) {
		body := api.CronJobEnabledActionBody{}
		if in.Body != nil {
			body = *in.Body
		}
		return handle(ctx, in.ProjectID, in.UID, in.Authorization, body, false, false)
	})
	huma.Register(h, huma.Operation{OperationID: "resumeCronJob", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}/resume"}, func(ctx context.Context, in *api.CronJobResumeRequest) (*api.CronJobResponse, error) {
		body := api.CronJobResumeBody{}
		if in.Body != nil {
			body = *in.Body
		}
		return handle(ctx, in.ProjectID, in.UID, in.Authorization, api.CronJobEnabledActionBody{Actor: body.Actor, ExpectedEventUID: body.ExpectedEventUID}, true, body.DiscardMissed)
	})
}
