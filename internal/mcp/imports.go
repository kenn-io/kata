package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// ImportIssuesInput carries a bounded structured issue batch.
type ImportIssuesInput struct {
	Project string                       `json:"project,omitempty"`
	Source  string                       `json:"source"`
	Items   []generated.ImportIssueInput `json:"items"`
}

// ImportIssuesOutput reports structured issue import results.
type ImportIssuesOutput struct {
	Project ProjectIdentity             `json:"project"`
	Result  generated.ImportBatchResult `json:"result"`
}

func registerImportTools(server *sdkmcp.Server, handlers toolHandlers) {
	addTool(server, "kata.import_issues", "Import issues", "Import a bounded structured issue batch into an in-scope project; existing external_id matches are updated in place.", toolHints(false, true, true), handlers.importIssues)
}

func (h toolHandlers) importIssues(ctx context.Context, _ *sdkmcp.CallToolRequest, input ImportIssuesInput) (*sdkmcp.CallToolResult, ImportIssuesOutput, error) {
	project, err := h.options.Scope.Project(ctx, h.options.Client, input.Project, false)
	if err != nil {
		return nil, ImportIssuesOutput{}, err
	}
	if strings.TrimSpace(input.Source) == "" {
		return nil, ImportIssuesOutput{}, errors.New("source is required")
	}
	if len(input.Items) == 0 || len(input.Items) > maximumResultLimit {
		return nil, ImportIssuesOutput{}, fmt.Errorf("items must contain between 1 and %d issues", maximumResultLimit)
	}
	response, err := h.options.Client.ImportIssues(ctx, &generated.ImportIssuesRequestOptions{
		PathParams: &generated.ImportIssuesPath{ProjectID: project.ID},
		Body:       &generated.ImportIssuesBody{Actor: h.options.Actor, Source: input.Source, Items: input.Items},
	})
	if err != nil {
		return nil, ImportIssuesOutput{}, err
	}
	return successResult(), ImportIssuesOutput{Project: project, Result: *response}, nil
}
