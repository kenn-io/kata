package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"
)

// Recheck each call: a long-lived MCP client can outlive a daemon replacement.
// A cached version could let an older daemon silently ignore new query fields.
func (h toolHandlers) requireDiscoveryAPI(ctx context.Context) error {
	health, err := h.options.Client.Health(ctx)
	if err != nil {
		return err
	}
	version := ""
	if health.APISchemaVersion != nil {
		version = strings.TrimSpace(*health.APISchemaVersion)
	}
	if !semver.IsValid("v"+version) || semver.Compare("v"+version, "v0.27.0") < 0 {
		return errors.New("paginated discovery requires daemon API 0.27.0 or newer")
	}
	return nil
}

func (h toolHandlers) list(ctx context.Context, _ *sdkmcp.CallToolRequest, input ListInput) (*sdkmcp.CallToolResult, IssueListOutput, error) {
	limit, err := boundedLimit(input.Limit)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	if input.Unowned && strings.TrimSpace(input.Owner) != "" {
		return nil, IssueListOutput{}, errors.New("owner and unowned are mutually exclusive")
	}
	if input.PriorityUnset && (input.Priority != nil || input.MaxPriority != nil) {
		return nil, IssueListOutput{}, errors.New("priority_unset is mutually exclusive with priority and max_priority")
	}
	sortOrder := input.Sort
	if sortOrder == "" {
		sortOrder = "created"
	}
	if sortOrder != "created" && sortOrder != "oldest" {
		return nil, IssueListOutput{}, errors.New("sort must be created or oldest")
	}
	var status *generated.ListIssuesQueryStatus
	if input.Status != "" {
		v := generated.ListIssuesQueryStatus(input.Status)
		if err := v.Validate(); err != nil {
			return nil, IssueListOutput{}, fmt.Errorf("status: %w", err)
		}
		status = &v
	}
	priority, err := priorityQuery(input.Priority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	maxPriority, err := priorityQuery(input.MaxPriority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	if input.PriorityUnset {
		priority = new("none")
	}
	if err := h.requireDiscoveryAPI(ctx); err != nil {
		return nil, IssueListOutput{}, err
	}
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	project, outputProjects := outputProjectScope(projects)
	out := IssueListOutput{Project: project, Projects: outputProjects, Issues: make([]IssueSummary, 0), Complete: true}
	if len(projects) == 0 {
		if input.Cursor != "" {
			return nil, IssueListOutput{}, errors.New("cursor scope changed; restart without a cursor")
		}
		if input.IncludeTotal {
			out.Total = new(int64(0))
		}
		return successResult(), out, nil
	}
	query := &generated.ListIssuesQuery{Status: status, Priority: priority, MaxPriority: maxPriority, Limit: new(int64(limit)), Sort: new(generated.ListIssuesQuerySort(sortOrder)), Cursor: optionalString(input.Cursor), IncludeTotal: optionalTrue(input.IncludeTotal), Unowned: optionalTrue(input.Unowned), Owner: optionalString(input.Owner), Label: compactStrings(input.Labels), ExcludeLabel: compactStrings(input.ExcludeLabels), Meta: compactStrings(input.Metadata)}
	if len(projects) == 1 && (h.options.Scope.Mode() == ScopeBound || strings.TrimSpace(input.Project) != "") {
		response, err := h.options.Client.ListIssues(ctx, &generated.ListIssuesRequestOptions{PathParams: &generated.ListIssuesPath{ProjectID: projects[0].ID}, Query: query})
		if err != nil {
			return nil, IssueListOutput{}, err
		}
		out.Complete = response.Complete
		out.NextCursor = response.NextCursor
		out.Total = response.Total
		for _, issue := range response.Issues {
			out.Issues = append(out.Issues, h.summaryFromIssueOut(projects[0], issue))
		}
	} else {
		var projectIDs []int64
		if h.options.Scope.Mode() == ScopeAllowlist {
			projectIDs = make([]int64, 0, len(projects))
			for _, p := range projects {
				projectIDs = append(projectIDs, p.ID)
			}
		}
		var globalStatus *generated.ListAllIssuesQueryStatus
		if status != nil {
			globalStatus = new(generated.ListAllIssuesQueryStatus(*status))
		}
		response, err := h.options.Client.ListAllIssues(ctx, &generated.ListAllIssuesRequestOptions{Query: &generated.ListAllIssuesQuery{Status: globalStatus, Priority: priority, MaxPriority: maxPriority, Limit: query.Limit, Sort: new(generated.ListAllIssuesQuerySort(sortOrder)), Cursor: query.Cursor, IncludeTotal: query.IncludeTotal, ProjectIds: projectIDs, Unowned: query.Unowned, Owner: query.Owner, Label: query.Label, ExcludeLabel: query.ExcludeLabel, Meta: query.Meta}})
		if err != nil {
			return nil, IssueListOutput{}, err
		}
		out.Complete = response.Complete
		out.NextCursor = response.NextCursor
		out.Total = response.Total
		allowed := projectIDSet(projects)
		for _, issue := range response.Issues {
			if _, ok := allowed[issue.ProjectID]; ok {
				out.Issues = append(out.Issues, summaryFromGlobalIssue(issue))
			}
		}
	}
	out.Truncated = !out.Complete
	return successResult(), out, nil
}
