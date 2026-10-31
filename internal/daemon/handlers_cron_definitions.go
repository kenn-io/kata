package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
)

func registerCronDefinitionHandlers(h huma.API, cfg ServerConfig) {
	registerCronJobHandlers(h, cfg)
	registerCronWorkflowHandlers(h, cfg)
}

func cronAPIError(ctx context.Context, cfg ServerConfig, err error, projectID int64, uid string, job bool) error {
	if apiErr, ok := errors.AsType[*api.APIError](err); ok {
		return apiErr
	}
	switch {
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(404, "not_found", "cron not found", "", nil)
	case errors.Is(err, cron.ErrInvalid):
		return api.NewError(400, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrCronConflict):
		data := map[string]any{}
		if uid != "" {
			if job {
				if current, readErr := cfg.DB.CronJob(ctx, projectID, uid); readErr == nil {
					data["current"] = api.CronJobFrom(current)
				}
			} else {
				if current, readErr := cfg.DB.CronWorkflow(ctx, projectID, uid); readErr == nil {
					data["current"] = api.CronWorkflowFrom(current)
				}
			}
		}
		return api.NewError(409, "cron_conflict", err.Error(), "refresh the current cron before retrying", data)
	case errors.Is(err, db.ErrFederatedReadOnly):
		return federationReadOnlyError(err)
	default:
		return internalAPIError(err)
	}
}

// checkCronLifecycleState refuses deleting a deleted definition or restoring
// a live one, either of which would otherwise emit a spurious event. The write
// that follows is a compare-and-swap on expected, so requiring the definition
// read here to be that version keeps this check atomic with the write.
func checkCronLifecycleState(current db.CronDefinition, expected string, deleting bool) error {
	if current.DefinitionEventUID != expected || deleting == (current.DeletedAt != nil) {
		return db.ErrCronConflict
	}
	return nil
}

// cronJobWriteContext preserves ordinary attribution while narrowing enrollment
// authority to direct project-scoped push credentials and their bound actor.
func cronJobWriteContext(ctx context.Context, cfg ServerConfig, projectID int64, authz, actor, operationID string) (context.Context, string, bool, error) {
	ctx, principal, err := resolveCapabilityPrincipal(ctx, cfg, projectID, authz,
		api.ClaimActionBody{Holder: actor}, federationTransportOperation(operationID), true, "push", false)
	if err != nil {
		return ctx, "", false, err
	}
	if principal.Enrollment {
		fed := principal.Federation
		if fed.ScopedProjectID != projectID || fed.RelayBindingUID != "" || fed.RelayProtocolVersion != 0 {
			return ctx, "", true, federationCredentialDenied()
		}
		if strings.TrimSpace(actor) != "" && strings.TrimSpace(actor) != fed.Actor {
			return ctx, "", true, federationCredentialDenied()
		}
		actor = fed.Actor
	}
	ctx, actor, err = cronWriteContext(ctx, actor)
	return ctx, actor, principal.Enrollment, err
}

func checkEnrollmentCronJob(ctx context.Context, cfg ServerConfig, projectID int64, id string, definition cron.JobDefinition) error {
	if cron.IsRecurringIssueJob(definition) {
		return nil
	}
	if id != "" {
		current, err := cfg.DB.CronJob(ctx, projectID, id)
		if err == nil && cron.IsRecurringIssueJob(current.Definition) {
			return nil
		}
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return cronAPIError(ctx, cfg, err, projectID, id, true)
		}
	}
	return federationCredentialDenied()
}

func registerCronJobHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron/jobs"
	put := func(ctx context.Context, projectID int64, id string, body api.PutCronJobBody, deleted, enrollment bool) (*api.CronJobResponse, error) {
		if enrollment {
			if err := checkEnrollmentCronJob(ctx, cfg, projectID, id, body.Definition.Native()); err != nil {
				return nil, err
			}
		}
		if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
			return nil, err
		}
		value, events, err := cfg.DB.PutCronJob(ctx, db.PutCronJob{ProjectID: projectID, UID: id, ExpectedEventUID: body.ExpectedEventUID, Name: body.Name, Definition: body.Definition.Native(), Actor: body.Actor, Deleted: deleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, true)
		}

		cfg.Publish().Events(projectID, events)
		out := &api.CronJobResponse{}
		out.Body.Job = api.CronJobFrom(value)
		out.Body.Events = events
		return out, nil
	}
	huma.Register(h, huma.Operation{OperationID: "createCronJob", Method: http.MethodPost, Path: path, DefaultStatus: 201}, func(ctx context.Context, in *api.CreateCronJobRequest) (*api.CronJobResponse, error) {
		ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, in.ProjectID, in.Authorization, in.Body.Actor, "createCronJob")
		if err != nil {
			return nil, err
		}
		in.Body.Actor = actor
		return put(ctx, in.ProjectID, "", in.Body, false, enrollment)
	})
	huma.Register(h, huma.Operation{OperationID: "replaceCronJob", Method: http.MethodPut, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.ReplaceCronJobRequest) (*api.CronJobResponse, error) {
		ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, in.ProjectID, in.Authorization, in.Body.Actor, "replaceCronJob")
		if err != nil {
			return nil, err
		}
		in.Body.Actor = actor
		return put(ctx, in.ProjectID, in.UID, in.Body, false, enrollment)
	})
	huma.Register(h, huma.Operation{OperationID: "showCronJob", Method: http.MethodGet, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.CronDefinitionRequest) (*api.CronJobResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		value, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
		}
		out := &api.CronJobResponse{}
		out.Body.Job = api.CronJobFrom(value)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronJobs", Method: http.MethodGet, Path: path}, func(ctx context.Context, in *api.ListCronDefinitionsRequest) (*api.ListCronJobsResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		values, err := cfg.DB.ListCronJobs(ctx, db.CronList{ProjectID: in.ProjectID, IncludeDeleted: in.IncludeDeleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", true)
		}
		out := &api.ListCronJobsResponse{}
		out.Body.Jobs = []api.CronJob{}
		for _, value := range values {
			out.Body.Jobs = append(out.Body.Jobs, api.CronJobFrom(value))
		}
		return out, nil
	})
	for _, action := range []struct {
		name, method, suffix string
		deleted              bool
	}{{"archive", http.MethodDelete, "", true}, {"restore", http.MethodPost, "/restore", false}} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronJob", Method: action.method, Path: path + "/{cron_uid}" + action.suffix}, func(ctx context.Context, in *api.CronDefinitionActionRequest) (*api.CronJobResponse, error) {
			ctx, actor, enrollment, err := cronJobWriteContext(ctx, cfg, in.ProjectID, in.Authorization, in.Body.Actor, action.name+"CronJob")
			if err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			value, err := cfg.DB.CronJob(ctx, in.ProjectID, in.UID)
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
			}
			if enrollment && !cron.IsRecurringIssueJob(value.Definition) {
				return nil, federationCredentialDenied()
			}
			if err := checkCronLifecycleState(value.CronDefinition, in.Body.ExpectedEventUID, action.deleted); err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, true)
			}
			return put(ctx, in.ProjectID, in.UID, api.PutCronJobBody{Actor: actor, Name: value.Name, Definition: api.CronJobDefinitionFrom(value.Definition), ExpectedEventUID: in.Body.ExpectedEventUID}, action.deleted, enrollment)
		})
	}
}

func registerCronWorkflowHandlers(h huma.API, cfg ServerConfig) {
	path := "/api/v1/projects/{project_id}/cron/workflows"
	put := func(ctx context.Context, projectID int64, id string, body api.PutCronWorkflowBody, deleted bool) (*api.CronWorkflowResponse, error) {
		ctx, actor, err := cronWriteContext(ctx, body.Actor)
		if err != nil {
			return nil, err
		}
		if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
			return nil, err
		}
		value, event, err := cfg.DB.PutCronWorkflow(ctx, db.PutCronWorkflow{ProjectID: projectID, UID: id, ExpectedEventUID: body.ExpectedEventUID, Name: body.Name, Definition: body.Definition.Native(), Actor: actor, Deleted: deleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, projectID, id, false)
		}
		events := []db.Event{event}
		cfg.Publish().Events(projectID, events)
		out := &api.CronWorkflowResponse{}
		out.Body.Workflow = api.CronWorkflowFrom(value)
		out.Body.Events = events
		return out, nil
	}
	huma.Register(h, huma.Operation{OperationID: "createCronWorkflow", Method: http.MethodPost, Path: path, DefaultStatus: 201}, func(ctx context.Context, in *api.CreateCronWorkflowRequest) (*api.CronWorkflowResponse, error) {
		return put(ctx, in.ProjectID, "", in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "replaceCronWorkflow", Method: http.MethodPut, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.ReplaceCronWorkflowRequest) (*api.CronWorkflowResponse, error) {
		return put(ctx, in.ProjectID, in.UID, in.Body, false)
	})
	huma.Register(h, huma.Operation{OperationID: "showCronWorkflow", Method: http.MethodGet, Path: path + "/{cron_uid}"}, func(ctx context.Context, in *api.CronDefinitionRequest) (*api.CronWorkflowResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		value, err := cfg.DB.CronWorkflow(ctx, in.ProjectID, in.UID)
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, false)
		}
		out := &api.CronWorkflowResponse{}
		out.Body.Workflow = api.CronWorkflowFrom(value)
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "listCronWorkflows", Method: http.MethodGet, Path: path}, func(ctx context.Context, in *api.ListCronDefinitionsRequest) (*api.ListCronWorkflowsResponse, error) {
		if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		values, err := cfg.DB.ListCronWorkflows(ctx, db.CronList{ProjectID: in.ProjectID, IncludeDeleted: in.IncludeDeleted})
		if err != nil {
			return nil, cronAPIError(ctx, cfg, err, in.ProjectID, "", false)
		}
		out := &api.ListCronWorkflowsResponse{}
		out.Body.Workflows = []api.CronWorkflow{}
		for _, value := range values {
			out.Body.Workflows = append(out.Body.Workflows, api.CronWorkflowFrom(value))
		}
		return out, nil
	})
	for _, action := range []struct {
		name, method, suffix string
		deleted              bool
	}{{"archive", http.MethodDelete, "", true}, {"restore", http.MethodPost, "/restore", false}} {
		huma.Register(h, huma.Operation{OperationID: action.name + "CronWorkflow", Method: action.method, Path: path + "/{cron_uid}" + action.suffix}, func(ctx context.Context, in *api.CronDefinitionActionRequest) (*api.CronWorkflowResponse, error) {
			if _, err := attributedActor(ctx, in.Body.Actor); err != nil {
				return nil, err
			}
			if _, err := activeProjectByID(ctx, cfg.DB, in.ProjectID); err != nil {
				return nil, err
			}
			value, err := cfg.DB.CronWorkflow(ctx, in.ProjectID, in.UID)
			if err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, false)
			}
			if err := checkCronLifecycleState(value.CronDefinition, in.Body.ExpectedEventUID, action.deleted); err != nil {
				return nil, cronAPIError(ctx, cfg, err, in.ProjectID, in.UID, false)
			}
			return put(ctx, in.ProjectID, in.UID, api.PutCronWorkflowBody{Actor: in.Body.Actor, Name: value.Name, Definition: api.CronWorkflowDefinitionFrom(value.Definition), ExpectedEventUID: in.Body.ExpectedEventUID}, action.deleted)
		})
	}
}
