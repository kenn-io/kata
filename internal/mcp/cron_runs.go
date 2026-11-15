package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/teammate"
	"go.kenn.io/kata/internal/timeparse"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronRunsInput requests a bounded project history page.
type CronRunsInput struct {
	CronProjectInput
	Limit     *int64 `json:"limit,omitempty" jsonschema:"Results from 1 through 100; omit for 20"`
	BeforeUID string `json:"before_uid,omitempty" jsonschema:"next_before_uid from the preceding page"`
	JobUID    string `json:"job_uid,omitempty"`
}

// CronRunStatsInput selects a bounded execution-time window.
type CronRunStatsInput struct {
	CronProjectInput
	JobUID      string `json:"job_uid,omitempty"`
	WorkflowUID string `json:"workflow_uid,omitempty"`
	Since       string `json:"since,omitempty"`
	Until       string `json:"until,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Timezone    string `json:"timezone,omitempty"`
}

// CronRunObserveInput reports evidence, without process execution authority.
type CronRunObserveInput struct {
	CronIdentityInput
	ExpectedRevision           int64                              `json:"expected_revision" jsonschema:"Current run revision; 0 for a new caller-retained UID; identical retries are replayed"`
	Teammate                   *string                            `json:"teammate,omitempty" jsonschema:"Per-call teammate; empty suppresses the startup default without changing accountable actor"`
	JobUID                     *string                            `json:"job_uid,omitempty"`
	DefinitionEventUID         *string                            `json:"definition_event_uid,omitempty"`
	WorkflowUID                *string                            `json:"workflow_uid,omitempty"`
	WorkflowDefinitionEventUID *string                            `json:"workflow_definition_event_uid,omitempty"`
	IssueUID                   *string                            `json:"issue_uid,omitempty"`
	OccurrenceKey              *string                            `json:"occurrence_key,omitempty"`
	ExecutorLabel              *string                            `json:"executor_label,omitempty"`
	Status                     generated.ObserveCronRunBodyStatus `json:"status"`
	Summary                    generated.Summary                  `json:"summary"`
	StartedAt                  *time.Time                         `json:"started_at,omitempty"`
	EndedAt                    *time.Time                         `json:"ended_at,omitempty"`
}

func registerCronRunTools(server *sdkmcp.Server, h toolHandlers) {
	read := toolHints(true, false, false)
	addCronTool(server, "kata.cron_run_stats", "Summarize cron runs", "Read bounded statistics for retained runs in a time window.", read, h.cronRunStats)
	addCronTool(server, "kata.cron_runs", "List cron run evidence", "Read bounded run observations with job filtering and next_before_uid pagination. Evidence does not prove or launch execution.", read, h.cronRuns)
	addCronTool(server, "kata.cron_run", "Read cron run evidence", "Read one independently attributed run observation; never launches work or claims issue ownership.", read, h.cronRun)
	addCronTool(server, "kata.cron_run_observe", "Record cron run evidence", "Record evidence using a retained run UID and expected_revision (0 for new). Identical retries are replayed. Does not launch work, claim ownership, or change linked issue status.", toolHints(false, true, false), h.cronRunObserve)
}

// CronRunStatsOutput preserves nullable numbers from the daemon response.
type CronRunStatsOutput struct {
	generated.GetCronRunStatsResponse
	Buckets *[]generated.RunStatBucket `json:"buckets,omitzero"`
}

func (h toolHandlers) cronRunStats(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRunStatsInput) (*sdkmcp.CallToolResult, CronRunStatsOutput, error) {
	var err error
	for _, id := range []*string{&in.JobUID, &in.WorkflowUID} {
		if *id != "" {
			*id, err = cronUID(*id)
			if err != nil {
				return nil, CronRunStatsOutput{}, err
			}
		}
	}
	in.Since, in.Until, err = timeparse.Window(in.Since, in.Until, time.Now())
	if err != nil {
		return nil, CronRunStatsOutput{}, err
	}
	supported, err := h.daemonSupportsAPIVersion(ctx, api.CronRunStatsAPIVersion)
	if err != nil {
		return nil, CronRunStatsOutput{}, err
	}
	if !supported {
		return nil, CronRunStatsOutput{}, errors.New("cron run stats requires daemon API " + api.CronRunStatsAPIVersion + " or newer; upgrade the daemon")
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, CronRunStatsOutput{}, err
	}
	response, err := h.options.Client.GetCronRunStats(ctx, &generated.GetCronRunStatsRequestOptions{PathParams: &generated.GetCronRunStatsPath{ProjectID: p.ID}, Query: &generated.GetCronRunStatsQuery{JobUID: optionalString(in.JobUID), WorkflowUID: optionalString(in.WorkflowUID), Since: optionalString(in.Since), Until: optionalString(in.Until), Bucket: optionalString(in.Bucket), Timezone: optionalString(in.Timezone)}})
	if err != nil {
		return nil, CronRunStatsOutput{}, err
	}
	return successResult(), CronRunStatsOutput{GetCronRunStatsResponse: *response, Buckets: response.Buckets}, nil
}
func (h toolHandlers) cronRuns(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRunsInput) (*sdkmcp.CallToolResult, generated.ListCronRunsResponse, error) {
	limit := int64(defaultResultLimit)
	if in.Limit != nil {
		limit = *in.Limit
	}
	if limit < 1 || limit > maximumResultLimit {
		return nil, generated.ListCronRunsResponse{}, errors.New("limit must be from 1 through 100")
	}
	var err error
	if in.BeforeUID != "" {
		in.BeforeUID, err = cronUID(in.BeforeUID)
		if err != nil {
			return nil, generated.ListCronRunsResponse{}, err
		}
	}
	if in.JobUID != "" {
		in.JobUID, err = cronUID(in.JobUID)
		if err != nil {
			return nil, generated.ListCronRunsResponse{}, err
		}
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, generated.ListCronRunsResponse{}, err
	}
	return cronResult(h.options.Client.ListCronRuns(ctx, &generated.ListCronRunsRequestOptions{PathParams: &generated.ListCronRunsPath{ProjectID: p.ID}, Query: &generated.ListCronRunsQuery{Limit: &limit, JobUID: optionalString(in.JobUID), BeforeUID: optionalString(in.BeforeUID)}}))
}
func (h toolHandlers) cronRun(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronIdentityInput) (*sdkmcp.CallToolResult, generated.ShowCronRunResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, generated.ShowCronRunResponse{}, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, generated.ShowCronRunResponse{}, err
	}
	return cronResult(h.options.Client.ShowCronRun(ctx, &generated.ShowCronRunRequestOptions{PathParams: &generated.ShowCronRunPath{ProjectID: p.ID, RunUID: id}}))
}
func (h toolHandlers) cronRunObserve(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronRunObserveInput) (*sdkmcp.CallToolResult, generated.ObserveCronRunResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, generated.ObserveCronRunResponse{}, err
	}
	handle, err := teammate.Resolve(in.Teammate, h.options.Teammate)
	if err != nil {
		return nil, generated.ObserveCronRunResponse{}, err
	}
	raw, err := json.Marshal(in.Summary)
	if err != nil {
		return nil, generated.ObserveCronRunResponse{}, err
	}
	if _, err = cron.ParseSummary(raw); err != nil {
		return nil, generated.ObserveCronRunResponse{}, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, generated.ObserveCronRunResponse{}, err
	}
	return cronResult(h.options.Client.ObserveCronRun(ctx, &generated.ObserveCronRunRequestOptions{PathParams: &generated.ObserveCronRunPath{ProjectID: p.ID, RunUID: id}, Body: &generated.ObserveCronRunBody{Actor: &h.options.Actor, ExpectedRevision: in.ExpectedRevision, Teammate: optionalString(handle), JobUID: in.JobUID, DefinitionEventUID: in.DefinitionEventUID, WorkflowUID: in.WorkflowUID, WorkflowDefinitionEventUID: in.WorkflowDefinitionEventUID, IssueUID: in.IssueUID, OccurrenceKey: in.OccurrenceKey, ExecutorLabel: in.ExecutorLabel, Status: in.Status, Summary: in.Summary, StartedAt: in.StartedAt, EndedAt: in.EndedAt}}))
}
