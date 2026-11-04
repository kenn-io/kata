package daemon

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func registerCronJobEnabledHandlers(h huma.API, cfg ServerConfig, put func(context.Context, int64, string, api.PutCronJobBody, bool, bool) (*api.CronJobResponse, error)) {
	for _, action := range []struct {
		name    string
		enabled bool
	}{{"pause", false}, {"resume", true}} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronJob", Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/jobs/{cron_uid}/" + action.name}, func(ctx context.Context, in *api.CronJobEnabledActionRequest) (*api.CronJobResponse, error) {
			body := api.CronJobEnabledActionBody{}
			if in.Body != nil {
				body = *in.Body
			}
			ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, in.ProjectID, in.Authorization, body.Actor, action.name+"CronJob")
			if err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			current, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
			}
			if enrollment && !cron.IsRecurringIssueJob(current.Definition) {
				return nil, federationCredentialDenied()
			}
			if current.DeletedAt != nil || (body.ExpectedEventUID != "" && body.ExpectedEventUID != current.DefinitionEventUID) {
				return nil, cronAPIError(ctx, cfg, db.ErrCronConflict, in.ProjectID, in.UID, true)
			}
			definition := current.Definition
			definition.Enabled = action.enabled
			write := api.PutCronJobBody{Actor: actor, Name: current.Name, Definition: api.CronJobDefinitionFrom(definition), ExpectedEventUID: current.DefinitionEventUID}
			// Local replicas cannot prove a hub no-op. A legacy conditional PUT keeps
			// old hubs usable without a job GET that enrollment credentials cannot make.
			if out, handled, err := forwardCronJob(ctx, cfg, in.ProjectID, in.UID, http.MethodPut, "", &write, nil, enrollment); handled {
				return out, err
			}
			if current.Definition.Enabled == action.enabled {
				out := &api.CronJobResponse{}
				out.Body.Job = api.CronJobFrom(current)
				return out, nil
			}
			return put(ctx, in.ProjectID, in.UID, write, false, enrollment)
		})
	}
}
