package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
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
				return validateCronDefinition(ctx, cfg, in.ProjectID, in.RawBody, in.From, "", false)
			})
		}
		// RawBody avoids framework coercion/422s. Publish the envelope schema while
		// leaving all decoding and stage-specific document errors to the handler.
		body.Content["application/json"] = &huma.MediaType{Schema: schema}
	}
}

func validateCronDefinition(ctx context.Context, cfg ServerConfig, projectID int64, raw []byte, fromText, countText string, job bool) (*api.CronValidationResponse, error) {
	invalid := func(message string) error { return api.NewError(400, "validation", message, "", nil) }
	var fields map[string]jsontext.Value
	if jsontext.Value(raw).Kind() != '{' {
		return nil, invalid("request body must be a JSON object")
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, invalid("invalid request body: " + err.Error())
	}
	var body struct {
		Name             string         `json:"name"`
		Definition       jsontext.Value `json:"definition"`
		UID              string         `json:"uid,omitempty"`
		ExpectedEventUID string         `json:"expected_event_uid,omitempty"`
		Actor            string         `json:"actor,omitempty"`
	}
	// JSON null is not a string, even though Go's decoder accepts it as zero.
	for key, value := range fields {
		switch key {
		case "definition":
		case "name", "uid", "expected_event_uid", "actor":
			if value.Kind() != '"' {
				return nil, invalid(key + " must be a string")
			}
		default:
			return nil, invalid("unknown request member: " + key)
		}
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, invalid(err.Error())
	}
	body.UID = strings.ToUpper(body.UID)
	if body.UID != "" && !uid.Valid(body.UID) {
		return nil, invalid("invalid UID")
	}
	from := time.Now().UTC()
	if fromText != "" {
		parsed, err := time.Parse(time.RFC3339, fromText)
		if err != nil {
			return nil, invalid("from must be RFC3339")
		}
		from = parsed
	}
	count := 5
	if countText != "" {
		parsed, err := strconv.Atoi(countText)
		if err != nil || parsed < 1 || parsed > 100 {
			return nil, invalid("count must be between 1 and 100")
		}
		count = parsed
	}
	input := db.CronValidationInput{ProjectID: projectID, UID: body.UID, ExpectedEventUID: body.ExpectedEventUID, Name: body.Name, Raw: body.Definition, From: from, Count: count}
	var result db.CronValidation
	var err error
	if job {
		result, err = cfg.DB.ValidateCronJob(ctx, input)
	} else {
		result, err = cfg.DB.ValidateCronWorkflow(ctx, input)
	}
	if err != nil {
		return nil, cronAPIError(ctx, cfg, err, projectID, body.UID, job)
	}
	return &api.CronValidationResponse{Body: api.CronValidationFrom(result)}, nil
}
