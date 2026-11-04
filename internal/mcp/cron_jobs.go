package mcpserver

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/pkg/client/generated"
)

// CronJobCreateInput creates one caller-identified definition.
type CronJobCreateInput struct {
	CronIdentityInput
	Name       string                      `json:"name"`
	Definition generated.CronJobDefinition `json:"definition"`
	ParentRef  string                      `json:"parent_ref,omitempty" jsonschema:"Optional in-project parent issue reference; must agree with definition.issue.parent_uid"`
}

// CronJobUpdateInput requires explicit compare-and-swap authority.
type CronJobUpdateInput struct {
	CronJobCreateInput
	ExpectedEventUID string `json:"expected_event_uid"`
}

func (h toolHandlers) cronJobs(ctx context.Context, request *sdkmcp.CallToolRequest, in CronListInput) (*sdkmcp.CallToolResult, *CronJobsOutput, error) {
	if err := validateCronReadArguments(request, false); err != nil {
		return nil, nil, err
	}
	projects, ids, global, err := h.cronReadScope(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	out := &CronJobsOutput{Jobs: []generated.CronJob{}}
	if len(projects) == 0 {
		return cronResult(out, nil)
	}
	if global {
		response, err := h.options.Client.ListAllCronJobs(ctx, &generated.ListAllCronJobsRequestOptions{Query: &generated.ListAllCronJobsQuery{ProjectIds: ids, IncludeDeleted: &in.IncludeDeleted}})
		if err != nil {
			return nil, nil, err
		}
		out.Jobs = response.Jobs
		out.Projects = response.Projects
	} else {
		response, err := h.options.Client.ListCronJobs(ctx, &generated.ListCronJobsRequestOptions{PathParams: &generated.ListCronJobsPath{ProjectID: projects[0].ID}, Query: &generated.ListCronJobsQuery{IncludeDeleted: &in.IncludeDeleted}})
		if err != nil {
			return nil, nil, err
		}
		out.Jobs = response.Jobs
	}
	return cronResult(out, nil)
}

func (h toolHandlers) cronJob(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronIdentityInput) (*sdkmcp.CallToolResult, *generated.ShowCronJobResponse, error) {
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ShowCronJob(ctx, &generated.ShowCronJobRequestOptions{PathParams: &generated.ShowCronJobPath{ProjectID: p.ID, CronUID: id}}))
}
func (h toolHandlers) cronJobCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobCreateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	return h.cronPutJob(ctx, in, nil)
}
func (h toolHandlers) cronJobUpdate(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronJobUpdateInput) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	expected, err := cronExpected(in.ExpectedEventUID)
	if err != nil {
		return nil, nil, err
	}
	return h.cronPutJob(ctx, in.CronJobCreateInput, &expected)
}
func (h toolHandlers) cronPutJob(ctx context.Context, in CronJobCreateInput, expected *string) (*sdkmcp.CallToolResult, *generated.ReplaceCronJobResponse, error) {
	h = h.withLongRunningClient()
	id, err := cronUID(in.UID)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.createTarget(ctx, in.Project)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.ParentRef) != "" {
		parent, err := h.cronParent(ctx, p, in.ParentRef)
		if err != nil {
			return nil, nil, err
		}
		if in.Definition.Issue == nil {
			return nil, nil, errors.New("parent_ref requires definition.issue")
		}
		if in.Definition.Issue.ParentUID != nil && *in.Definition.Issue.ParentUID != parent {
			return nil, nil, errors.New("parent_ref disagrees with definition.issue.parent_uid")
		}
		in.Definition.Issue.ParentUID = &parent
	}
	raw, err := json.Marshal(in.Definition)
	if err != nil {
		return nil, nil, err
	}
	if _, err = cron.ParseJob(raw); err != nil {
		return nil, nil, err
	}
	return cronResult(h.options.Client.ReplaceCronJob(ctx, &generated.ReplaceCronJobRequestOptions{PathParams: &generated.ReplaceCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ReplaceCronJobBody{Actor: &h.options.Actor, Name: in.Name, Definition: in.Definition, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronParent(ctx context.Context, p ProjectIdentity, raw string) (string, error) {
	parsed, err := shortid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Project != "" && parsed.Project != p.Name {
		return "", errors.New("parent_ref must belong to the selected project")
	}
	ref := parsed.ShortID
	if parsed.ULID != "" {
		ref = parsed.ULID
	}
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: p.ID, Ref: ref}})
	if err != nil {
		return "", err
	}
	if shown.Issue.ProjectID != p.ID || (p.UID != "" && (shown.Issue.ProjectUID == nil || *shown.Issue.ProjectUID != p.UID)) {
		return "", fmt.Errorf("parent_ref resolved outside project %q", p.Name)
	}
	return shown.Issue.UID, nil
}
func (h toolHandlers) cronJobDelete(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.ArchiveCronJobResponse, error) {
	h = h.withLongRunningClient()
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
	return cronResult(h.options.Client.ArchiveCronJob(ctx, &generated.ArchiveCronJobRequestOptions{PathParams: &generated.ArchiveCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.ArchiveCronJobBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
func (h toolHandlers) cronJobRestore(ctx context.Context, _ *sdkmcp.CallToolRequest, in CronActionInput) (*sdkmcp.CallToolResult, *generated.RestoreCronJobResponse, error) {
	h = h.withLongRunningClient()
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
	return cronResult(h.options.Client.RestoreCronJob(ctx, &generated.RestoreCronJobRequestOptions{PathParams: &generated.RestoreCronJobPath{ProjectID: p.ID, CronUID: id}, Body: &generated.RestoreCronJobBody{Actor: &h.options.Actor, ExpectedEventUID: expected}}))
}
