package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func cronControls() []string { return []string{"job_pause", "project_pause"} }

func registerCronControlHandlers(h huma.API, cfg ServerConfig) {
	for _, action := range []struct {
		name, method string
		paused       bool
	}{
		{"pause", http.MethodPut, true}, {"resume", http.MethodDelete, false},
	} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronProject", Method: action.method, Path: "/api/v1/projects/{project_id}/cron/pause"}, func(ctx context.Context, in *api.CronProjectPauseRequest) (*api.CronProjectPauseResponse, error) {
			body := api.CronProjectPauseBody{}
			if in.Body != nil {
				body = *in.Body
			}
			ctx, actor, err := cronWriteContext(ctx, body.Actor)
			if err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			result, err := cfg.DB.SetCronProjectPause(ctx, db.SetCronProjectPauseIn{ProjectID: in.ProjectID, Actor: actor, Reason: body.Reason, Paused: action.paused})
			if err != nil {
				if errors.Is(err, db.ErrFederatedSpokeUnsupported) {
					return nil, api.NewError(409, "federated_read_only", "project cron policy belongs to the owning hub", "Run the same command against the project's owning hub, for example with --daemon <hub-name>.", nil)
				}
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
			}
			if result.Changed {
				cfg.Publish().Event(in.ProjectID, result.Event)
			}
			out := &api.CronProjectPauseResponse{}
			out.Body.ProjectPause = api.CronProjectPauseFromMetadata(result.Project.Metadata)
			out.Body.Changed = result.Changed
			return out, nil
		})
	}
}
