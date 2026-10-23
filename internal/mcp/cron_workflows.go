package mcpserver

import (
	"context"
	"encoding/json/v2"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronWorkflowCreateInput retains the identity before the first network write.
type CronWorkflowCreateInput struct {
	CronIdentityInput
	Name       string                           `json:"name"`
	Definition generated.CronWorkflowDefinition `json:"definition"`
}

// CronWorkflowUpdateInput replaces a complete definition at its visible event.
type CronWorkflowUpdateInput struct {
	CronWorkflowCreateInput
	ExpectedEventUID string `json:"expected_event_uid"`
}

func registerCronWorkflowTools(server *sdkmcp.Server, h toolHandlers) {
	read := toolHints(true, false, false)
	addCronTool(server, "kata.cron_workflows", "List cron workflows", "List portable workflow definitions. Workflows require an external execution adapter; these tools launch no processes.", read, h.cronWorkflows)
	addCronTool(server, "kata.cron_workflow", "Read cron workflow", "Read a retained workflow UID and its current definition_event_uid, including a reversible tombstone.", read, h.cronWorkflow)
	addCronTool(server, "kata.cron_workflow_create", "Create cron workflow", "Create a typed workflow using a UID retained before the first call. No process or execution adapter is launched.", toolHints(false, false, false), h.cronWorkflowCreate)
	addCronTool(server, "kata.cron_workflow_update", "Replace cron workflow", "Replace a complete workflow using its current expected_event_uid; stale values preserve daemon conflicts.", toolHints(false, true, false), h.cronWorkflowUpdate)
	addCronTool(server, "kata.cron_workflow_delete", "Archive cron workflow", "Reversibly tombstone a workflow using its current expected_event_uid. In-use workflow conflicts are preserved.", toolHints(false, true, false), h.cronWorkflowDelete)
	addCronTool(server, "kata.cron_workflow_restore", "Restore cron workflow", "Restore a workflow tombstone using its current expected_event_uid and the daemon's dependency checks.", toolHints(false, false, false), h.cronWorkflowRestore)
}
func (h toolHandlers) cronWorkflows(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronListInput) (*sdkmcp.CallToolResult, *generated.ListCronWorkflowsResponse, error) {
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ListCronWorkflows(ctx, &generated.ListCronWorkflowsRequestOptions{PathParams: &generated.ListCronWorkflowsPath{ProjectID: p.ID}, Query: &generated.ListCronWorkflowsQuery{IncludeDeleted: &in.IncludeDeleted}}))
}
func (h toolHandlers) cronWorkflow(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronIdentityInput) (*sdkmcp.CallToolResult, *generated.ShowCronWorkflowResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ShowCronWorkflow(ctx, &generated.ShowCronWorkflowRequestOptions{PathParams: &generated.ShowCronWorkflowPath{ProjectID: p.ID, CronUID: id}}))
}
func (h toolHandlers) cronWorkflowCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronWorkflowCreateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronWorkflowResponse, error) {
	return h.cronPutWorkflow(ctx, in, nil)
}
func (h toolHandlers) cronWorkflowUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronWorkflowUpdateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronWorkflowResponse, error) {
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	return h.cronPutWorkflow(ctx, in.CronWorkflowCreateInput, &expected)
}
func (h toolHandlers) cronPutWorkflow(ctx context.Context, in CronWorkflowCreateInput, expected *string) (*sdkmcp.CallToolResult, *generated.ReplaceCronWorkflowResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(in.Definition)
	if err != nil {
		return nil, nil, err
	}
	if _, err = cron.ParseWorkflow(raw); err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ReplaceCronWorkflow(ctx, &generated.ReplaceCronWorkflowRequestOptions{PathParams: &generated.ReplaceCronWorkflowPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ReplaceCronWorkflowBody{Actor: &h.options.Actor, Name: in.Name, Definition: in.Definition, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronWorkflowDelete(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.ArchiveCronWorkflowResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ArchiveCronWorkflow(ctx, &generated.ArchiveCronWorkflowRequestOptions{PathParams: &generated.ArchiveCronWorkflowPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ArchiveCronWorkflowBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronWorkflowRestore(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.RestoreCronWorkflowResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.RestoreCronWorkflow(ctx, &generated.RestoreCronWorkflowRequestOptions{PathParams: &generated.RestoreCronWorkflowPath{ProjectID: p.ID, CronUID: id}, Body: &generated.RestoreCronWorkflowBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
