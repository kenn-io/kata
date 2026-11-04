package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronValidationInput leaves document rules to the daemon, including malformed
// definitions. No UID or mutation actor is allocated for validation.
type CronValidationInput struct {
	CronProjectInput
	UID              string `json:"uid,omitempty"`
	ExpectedEventUID string `json:"expected_event_uid,omitempty"`
	Name             string `json:"name,omitempty"`
	Definition       any    `json:"definition,omitempty" jsonschema:"Unsaved definition; malformed documents return staged validation errors"`
}

// CronJobValidationInput also selects a parent and hypothetical future instants.
type CronJobValidationInput struct {
	CronValidationInput
	ParentRef string `json:"parent_ref,omitempty"`
	Count     *int   `json:"count,omitempty" jsonschema:"Number of next fires, 1 through 100; defaults to 5"`
	From      string `json:"from,omitempty" jsonschema:"RFC3339 preview start; defaults to daemon now"`
}

// CronValidationOutput mirrors the generated response with an unrestricted JSON
// definition schema. RawMessage otherwise infers a byte-array MCP schema.
type CronValidationOutput struct {
	Valid      bool                              `json:"valid"`
	Name       string                            `json:"name"`
	Definition any                               `json:"definition"`
	Errors     []generated.CronValidationError   `json:"errors"`
	Warnings   []generated.CronValidationWarning `json:"warnings"`
	Schedule   *generated.SchedulePreview        `json:"schedule,omitempty"`
}

func registerCronValidationTools(server *sdkmcp.Server, h toolHandlers) {
	read := toolHints(true, false, false)
	addCronTool(server, "kata.cron_job_validate", "Validate cron job", "Validate an unsaved job and preview hypothetical next fires without saving. Invalid definitions and revision conflicts return valid:false with staged errors. Reads the selected daemon's current state without forwarding.", read, h.cronJobValidate, "definition")
	addCronTool(server, "kata.cron_workflow_validate", "Validate cron workflow", "Validate an unsaved workflow without saving. Returns the normalized document and staged errors without requiring a mutation actor or allocating a UID.", read, h.cronWorkflowValidate, "definition")
}

func cronValidationBody(in CronValidationInput) (*generated.CronValidationBody, error) {
	if in.UID != "" {
		id, err := cronUID(in.UID)
		if err != nil {
			return nil, err
		}
		in.UID = id
	}
	if in.ExpectedEventUID != "" {
		expected, err := cronExpected(in.ExpectedEventUID)
		if err != nil {
			return nil, err
		}
		in.ExpectedEventUID = expected
	}
	raw, err := json.Marshal(in.Definition)
	if err != nil {
		return nil, err
	}
	body := &generated.CronValidationBody{Name: in.Name, Definition: raw}
	if in.UID != "" {
		body.UID = &in.UID
	}
	if in.ExpectedEventUID != "" {
		body.ExpectedEventUID = &in.ExpectedEventUID
	}
	return body, nil
}

func cronValidationResult(value *generated.CronValidationResult, err error) (*sdkmcp.CallToolResult, *CronValidationOutput, error) {
	if err != nil {
		return nil, nil, err
	}
	var definition any
	if err := json.Unmarshal(value.Definition, &definition, jsonutil.PreserveNumberLiterals()); err != nil {
		return nil, nil, err
	}
	return successResult(), &CronValidationOutput{Valid: value.Valid, Name: value.Name, Definition: definition, Errors: value.Errors, Warnings: value.Warnings, Schedule: value.Schedule}, nil
}

func (h toolHandlers) cronJobValidate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobValidationInput) (*sdkmcp.CallToolResult, *CronValidationOutput, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.ParentRef) != "" {
		parent, err := h.cronParent(ctx, project, in.ParentRef)
		if err != nil {
			return nil, nil, err
		}
		definition, _ := in.Definition.(map[string]any)
		issue, ok := definition["issue"].(map[string]any)
		if !ok {
			return nil, nil, errors.New("parent_ref requires definition.issue")
		}
		if existing, ok := issue["parent_uid"]; ok && existing != parent {
			return nil, nil, errors.New("parent_ref disagrees with definition.issue.parent_uid")
		}
		issue["parent_uid"] = parent
	}
	body, err := cronValidationBody(in.CronValidationInput)
	if err != nil {
		return nil, nil, err
	}
	count := 5
	if in.Count != nil {
		count = *in.Count
	}
	countText := strconv.Itoa(count)
	return cronValidationResult(h.options.Client.ValidateCronJob(ctx, &generated.ValidateCronJobRequestOptions{PathParams: &generated.ValidateCronJobPath{ProjectID: project.ID}, Query: &generated.ValidateCronJobQuery{Count: &countText, From: &in.From}, Body: body}))
}

func (h toolHandlers) cronWorkflowValidate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronValidationInput) (*sdkmcp.CallToolResult, *CronValidationOutput, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	body, err := cronValidationBody(in)
	if err != nil {
		return nil, nil, err
	}
	return cronValidationResult(h.options.Client.ValidateCronWorkflow(ctx, &generated.ValidateCronWorkflowRequestOptions{PathParams: &generated.ValidateCronWorkflowPath{ProjectID: project.ID}, Body: body}))
}
