package mcpserver

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronRuntimeRequestInput retains caller identity across ambiguous retries.
type CronRuntimeRequestInput struct {
	CronIdentityInput
	RequestUID string `json:"request_uid" jsonschema:"Fresh request ULID; generate just before first submission and retain for retry"`
	TTLSeconds *int64 `json:"ttl_seconds,omitempty" jsonschema:"Lifetime in seconds, 60..86400; default 600. Retain the same TTL for retry"`
}

// CronRequestListInput selects a current derived state without changing requests.
type CronRequestListInput struct {
	CronProjectInput
	State string `json:"state,omitempty" jsonschema:"Filter pending, answered or expired requests"`
}

func registerCronRequestTools(server *sdkmcp.Server, h toolHandlers) {
	addCronTool(server, "kata.cron_job_request_run", "Request cron job run", "Ask an optional runtime to run an enabled adapter execute job now. Root-only; requires an unpaused project. Retain request_uid and TTL for retry. Admission launches nothing itself.", toolHints(false, false, false), h.cronJobRequestRun)
	addCronTool(server, "kata.cron_run_request_cancel", "Request cron run cancellation", "Ask optional runtimes to cancel a currently running run. Root-only; allowed while cron is paused. Retain request_uid and TTL for retry. Admission does not itself terminate anything.", toolHints(false, false, false), h.cronRunRequestCancel)
	addCronTool(server, "kata.cron_request_list", "List cron requests", "Read locally retained advisory requests and their current answer state. State may change with run evidence; expired requests disappear after collection.", toolHints(true, false, false), h.cronRequestList)
}
func (h toolHandlers) cronJobRequestRun(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRuntimeRequestInput) (*sdkmcp.CallToolResult, *generated.RequestCronJobRunResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	target, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	id, err := cronUID(in.RequestUID)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.RequestCronJobRun(ctx, &generated.RequestCronJobRunRequestOptions{PathParams: &generated.RequestCronJobRunPath{ProjectID: project.ID, CronUID: target}, Body: &generated.CronRequestBody{UID: id, Actor: &h.options.Actor, TTLSeconds: in.TTLSeconds}}))
}
func (h toolHandlers) cronRunRequestCancel(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRuntimeRequestInput) (*sdkmcp.CallToolResult, *generated.RequestCronRunCancelResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	target, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	id, err := cronUID(in.RequestUID)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.RequestCronRunCancel(ctx, &generated.RequestCronRunCancelRequestOptions{PathParams: &generated.RequestCronRunCancelPath{ProjectID: project.ID, RunUID: target}, Body: &generated.CronRequestBody{UID: id, Actor: &h.options.Actor, TTLSeconds: in.TTLSeconds}}))
}
func (h toolHandlers) cronRequestList(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRequestListInput) (*sdkmcp.CallToolResult, *generated.ListCronRequestsResponse, error) {
	project, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	query := &generated.ListCronRequestsQuery{}
	if in.State != "" {
		query.State = new(generated.ListCronRequestsQueryState(in.State))
	}
	return cronResult(h.options.Client.ListCronRequests(ctx, &generated.ListCronRequestsRequestOptions{PathParams: &generated.ListCronRequestsPath{ProjectID: project.ID}, Query: query}))
}
