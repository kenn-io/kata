package daemon

import (
	"context"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerCronValidationHandlers(h huma.API, cfg ServerConfig) {
	schema := h.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[api.CronValidationBody](), true, "")
	for _, job := range []bool{true, false} {
		resource, operation := "workflows", "validateCronWorkflow"
		if job {
			resource, operation = "jobs", "validateCronJob"
		}
		body := &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{}}
		op := huma.Operation{OperationID: operation, Method: http.MethodPost, Path: "/api/v1/projects/{project_id}/cron/" + resource + "/validate", RequestBody: body, MaxBodyBytes: 1 << 20, SkipValidateBody: true, Errors: []int{400, 404, 413}}
		if job {
			huma.Register(h, op, func(ctx context.Context, in *api.ValidateCronJobRequest) (*api.CronValidationResponse, error) {
				return validateCronDefinition(ctx, cfg, in.ProjectID, in.RawBody, in.From, in.Count, true)
			})
		} else {
			huma.Register(h, op, func(ctx context.Context, in *api.ValidateCronWorkflowRequest) (*api.CronValidationResponse, error) {
				return validateCronDefinition(ctx, cfg, in.ProjectID, in.RawBody, in.From, 5, false)
			})
		}
		// RawBody avoids framework coercion/422s. Publish the envelope schema while
		// leaving all decoding and stage-specific document errors to the handler.
		body.Content["application/json"] = &huma.MediaType{Schema: schema}
	}
}

func validateCronDefinition(ctx context.Context, cfg ServerConfig, projectID int64, raw []byte, fromText string, count int, job bool) (*api.CronValidationResponse, error) {
	invalid := func(message string) error { return api.NewError(400, "validation", message, "", nil) }
	fields, err := api.DecodeCronValidationEnvelope(raw)
	if err != nil {
		return nil, invalid(err.Error())
	}
	from := time.Now().UTC()
	if fromText != "" {
		parsed, err := time.Parse(time.RFC3339, fromText)
		if err != nil {
			return nil, invalid("from must be RFC3339")
		}
		if _, err := parsed.UTC().MarshalText(); err != nil {
			return nil, invalid("from must represent an RFC3339 UTC instant")
		}
		from = parsed
	}
	input := db.CronValidationInput{ProjectID: projectID, UID: fields.String("uid"), ExpectedEventUID: fields.String("expected_event_uid"), Name: fields.String("name"), Raw: fields["definition"], From: from, Count: count}
	var result db.CronValidation
	if job {
		result, err = cfg.DB.ValidateCronJob(ctx, input)
	} else {
		result, err = cfg.DB.ValidateCronWorkflow(ctx, input)
	}
	if err != nil {
		return nil, cronAPIError(ctx, cfg, err, projectID, input.UID, job)
	}
	return &api.CronValidationResponse{Body: api.CronValidationResult{Valid: result.Valid, Name: result.Name, Definition: api.JSONNullableRawObject(result.Definition), Errors: result.Errors, Warnings: result.Warnings, Schedule: result.Schedule}}, nil
}
