package mcpserver

import (
	"context"
	"errors"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

// A historical route may resolve into another project and allocate a different
// short ID. Resolve the returned identity before building references or asking
// for destructive confirmation; the route remains only a lookup hint.
func (h toolHandlers) currentIssueProject(ctx context.Context, route ProjectIdentity, issue generated.Issue) (ProjectIdentity, error) {
	projectUID := ""
	if issue.ProjectUID != nil {
		projectUID = *issue.ProjectUID
	}
	sameID := issue.ProjectID == 0 || issue.ProjectID == route.ID
	sameUID := projectUID == "" || route.UID == "" || projectUID == route.UID
	if sameID && sameUID {
		// Legacy response shapes and bound callers can omit project identity fields.
		if route.UID == "" {
			route.UID = projectUID
		}
		return route, nil
	}
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, true)
	if err != nil {
		return ProjectIdentity{}, err
	}
	for _, project := range projects {
		if issue.ProjectID != 0 && project.ID != issue.ProjectID {
			continue
		}
		if projectUID != "" && project.UID != projectUID {
			continue
		}
		return project, nil
	}
	return ProjectIdentity{}, errors.New("issue's current project is outside the MCP startup scope")
}

func (h toolHandlers) mutationResult(ctx context.Context, route ProjectIdentity, issue generated.Issue, changed bool, reused *bool, event *generated.Event) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, err := h.currentIssueProject(ctx, route, issue)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, issue, changed, reused, event), nil
}
