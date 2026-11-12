package mcpserver

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/jsonutil"
	"go.kenn.io/kata/internal/uid"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronProjectInput selects one project inside the MCP startup scope.
type CronProjectInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project name; required outside a single bound project"`
}

// CronListInput includes reversible tombstones when requested.
type CronListInput struct {
	CronProjectInput
	IncludeDeleted bool `json:"include_deleted,omitempty"`
}

// CronIdentityInput selects a retained definition UID.
type CronIdentityInput struct {
	CronProjectInput
	UID string `json:"uid" jsonschema:"Stable UID retained before creation; obtain one with kata.cron_new_uid"`
}

// CronActionInput requires the caller's current definition event.
type CronActionInput struct {
	CronIdentityInput
	ExpectedEventUID string `json:"expected_event_uid" jsonschema:"Current definition_event_uid; stale values conflict without overwriting"`
}

// CronUIDOutput contains an identity without allocating any daemon state.
type CronUIDOutput struct {
	UID string `json:"uid"`
}

func registerCronTools(server *sdkmcp.Server, h toolHandlers) {
	registerCronWorkflowTools(server, h)
	registerCronRunTools(server, h)
	read := toolHints(true, false, false)
	addCronTool(server, "kata.cron_new_uid", "Generate cron UID", "Generate and retain a UID before the first create or observe call. Creates and reserves nothing; the UID is unbound until creation. Reuse it after a lost response.", nonIdempotent(toolHints(true, false, false)), h.cronNewUID)
	addCronTool(server, "kata.cron_capabilities", "Read cron capabilities", "Read the selected project's live cron capabilities under the configured daemon authority.", read, h.cronCapabilities)
	addCronTool(server, "kata.cron_jobs", "List cron jobs", "List native notification and adapter execution definitions. Execution jobs need an external adapter; listing launches no work.", read, h.cronJobs)
	addCronTool(server, "kata.cron_job", "Read cron job", "Read one retained job UID, including its current definition_event_uid and reversible tombstone.", read, h.cronJob)
	addCronTool(server, "kata.cron_job_inputs", "Resolve cron job inputs", "Read resolved declarations, fixed values, and launch errors for one job.", read, h.cronJobInputs)
	addCronTool(server, "kata.cron_job_create", "Create cron job", "Create a typed job with a caller-retained UID from kata.cron_new_uid. Native notification jobs use daemon delivery; execution jobs require an external adapter. Retry retains UID and daemon conflicts.", toolHints(false, false, false), h.cronJobCreate)
	addCronTool(server, "kata.cron_job_update", "Replace cron job", "Replace a complete definition using its current expected_event_uid. Native parent bindings cannot change. Never launches an execution adapter.", toolHints(false, true, false), h.cronJobUpdate)
	addCronTool(server, "kata.cron_job_delete", "Archive cron job", "Reversibly tombstone a cron definition using its current expected_event_uid.", toolHints(false, true, false), h.cronJobDelete)
	addCronTool(server, "kata.cron_job_restore", "Restore cron job", "Restore a cron tombstone using its current expected_event_uid; preserves daemon dependency checks.", toolHints(false, false, false), h.cronJobRestore)
}
func (h toolHandlers) cronNewUID(context.Context, *sdkmcp.CallToolRequest, CronProjectInput) (*sdkmcp.CallToolResult, CronUIDOutput, error) {
	id, err := kataclient.NewCronUID()
	return successResult(), CronUIDOutput{UID: id}, err
}
func (h toolHandlers) cronCapabilities(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronProjectInput) (*sdkmcp.CallToolResult, generated.GetCronCapabilitiesResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, generated.GetCronCapabilitiesResponse{}, err
	}
	return cronResult(h.options.Client.GetCronCapabilities(ctx, &generated.GetCronCapabilitiesRequestOptions{PathParams: &generated.GetCronCapabilitiesPath{ProjectID: project.ID}}))
}
func cronResult[T any](value *T, err error) (*sdkmcp.CallToolResult, T, error) {
	if err != nil {
		var zero T
		return nil, zero, err
	}
	return successResult(), *value, nil
}
func cronUID(value string) (string, error) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !uid.Valid(value) {
		return "", errors.New("cron UID must be a full ULID")
	}
	return value, nil
}
func cronExpected(value string) (string, error) {
	id, err := cronUID(value)
	if err != nil {
		return "", fmt.Errorf("expected_event_uid: %w", err)
	}
	return id, nil
}

// cronOutputSchema replaces the generated union wrapper with its wire scalar schema.
func cronOutputSchema[Out any](name string) *jsonschema.Schema {
	schema := schemaFor[Out]()
	if name == "kata.cron_job_inputs" {
		inputs := schema.Properties["inputs"]
		if inputs == nil || inputs.Items == nil {
			panic("cron job inputs output schema lacks inputs items")
		}
		inputs.Items.Properties["value"] = &jsonschema.Schema{Types: []string{"string", "integer", "boolean"}}
	}
	return schema
}

// Preserve the presence of an optional scalar even when its JSON value is empty.
var cronInputMarshalers = json.WithMarshalers(json.MarshalFunc(func(input generated.CronResolvedInput) ([]byte, error) {
	type plainInput generated.CronResolvedInput
	return json.Marshal(struct {
		plainInput
		Value *generated.CronResolvedInput_Value `json:"value,omitzero"`
	}{plainInput: plainInput(input), Value: input.Value})
}))

// addCronTool retains original JSON bytes across validation. The SDK's generic
// typed adapter re-marshals a float64 validation copy and rounds opaque options
// and exact int64 evidence. Validate separately, then decode the original.
func addCronTool[In, Out any](server *sdkmcp.Server, name, title, description string, hints *sdkmcp.ToolAnnotations, handler sdkmcp.ToolHandlerFor[In, Out]) {
	inputSchema, outputSchema := inputSchemaFor[In](name), cronOutputSchema[Out](name)
	// Generated response pointers infer a nullable root. MCP requires an
	// object outputSchema; handler failures return IsError instead.
	if len(outputSchema.Types) == 2 && slices.Contains(outputSchema.Types, "object") && slices.Contains(outputSchema.Types, "null") {
		outputSchema.Type = "object"
		outputSchema.Types = nil
	}
	resolved, err := inputSchema.Resolve(nil)
	if err != nil {
		panic(err)
	}
	server.AddTool(&sdkmcp.Tool{Name: name, Title: title, Description: description, Annotations: hints, InputSchema: inputSchema, OutputSchema: outputSchema}, func(ctx context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		failure := func(err error) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{IsError: true, Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: err.Error()}}}, nil
		}
		raw := req.Params.Arguments
		if len(raw) == 0 {
			raw = jsontext.Value(`{}`)
		}
		var validation any
		if err := json.Unmarshal(raw, &validation); err != nil {
			return failure(err)
		}
		if err := resolved.Validate(validation); err != nil {
			return failure(err)
		}
		var input In
		if err := json.Unmarshal(raw, &input, jsonutil.PreserveNumberLiterals()); err != nil {
			return failure(err)
		}
		result, output, err := handler(ctx, req, input)
		if err != nil {
			return failure(err)
		}
		var encoded []byte
		if name == "kata.cron_job_inputs" {
			// Generated optional scalar wrappers must retain an explicitly set empty string.
			encoded, err = json.Marshal(output, cronInputMarshalers)
		} else {
			encoded, err = json.Marshal(output)
		}
		if err != nil {
			return failure(err)
		}
		if result == nil {
			result = &sdkmcp.CallToolResult{}
		}
		result.StructuredContent = jsontext.Value(encoded)
		result.Content = []sdkmcp.Content{&sdkmcp.TextContent{Text: string(encoded)}}
		return result, nil
	})
}
