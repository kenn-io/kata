package mcpserver

import (
	"context"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type FileEvidenceInput struct {
	Ref        string `json:"ref" jsonschema:"Issue reference"`
	EventUID   string `json:"event_uid,omitempty" jsonschema:"Close event UID; default effective current close"`
	Index      int64  `json:"index,omitempty" jsonschema:"Zero-based file-only evidence index"`
	Historical bool   `json:"historical,omitempty" jsonschema:"Request original bytes; never silently fall back to current"`
	Content    bool   `json:"content,omitempty" jsonschema:"Include complete base64 bytes"`
}

func (h toolHandlers) fileEvidence(ctx context.Context, _ *sdkmcp.CallToolRequest, in FileEvidenceInput) (*sdkmcp.CallToolResult, generated.FileEvidenceBody, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, in.Ref, false)
	if err != nil {
		return nil, generated.FileEvidenceBody{}, err
	}
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref}})
	if err != nil {
		return nil, generated.FileEvidenceBody{}, err
	}
	project, err = h.currentIssueProject(ctx, project, issueFromShow(shown.Issue))
	if err != nil {
		return nil, generated.FileEvidenceBody{}, err
	}
	result, err := h.fileOperationClient().ReadFileEvidence(ctx, &generated.ReadFileEvidenceRequestOptions{PathParams: &generated.ReadFileEvidencePath{ProjectID: project.ID, Ref: shown.Issue.UID}, Query: &generated.ReadFileEvidenceQuery{EventUID: optionalString(in.EventUID), Index: &in.Index, Historical: &in.Historical, Content: &in.Content}})
	if err != nil {
		return nil, generated.FileEvidenceBody{}, err
	}
	return successResult(), *result, nil
}

// Production MCP mounts/sidecars supply this existing long-operation transport.
func (h toolHandlers) fileOperationClient() *kataclient.Client {
	if h.options.LongRunningClient != nil {
		return h.options.LongRunningClient
	}
	return h.options.Client
}
