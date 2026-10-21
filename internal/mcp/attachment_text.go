package mcpserver

import (
	"context"
	"errors"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/kata/pkg/client/generated"
)

// AttachmentTextInput selects an issue's pinned attachment rendition.
type AttachmentTextInput struct {
	Issue      string `json:"issue"`
	Attachment string `json:"attachment"`
	MaxBytes   *int64 `json:"max_bytes,omitempty" jsonschema:"Maximum UTF-8 text bytes (1–16777216); defaults to 65536; capped at 1048576 for MCP response size"`
	Profile    string `json:"profile,omitempty"`
}

func registerAttachmentTools(server *sdkmcp.Server, h toolHandlers) {
	addTool(server, "kata.attachment_text", "Read attachment text", "Read a pinned Docbank attachment's text or its current rendition state. Does not trigger processing.", toolHints(true, false, false), h.attachmentText)
}
func (h toolHandlers) attachmentText(ctx context.Context, _ *sdkmcp.CallToolRequest, in AttachmentTextInput) (*sdkmcp.CallToolResult, generated.TextResult, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, in.Issue, false)
	if err != nil {
		return nil, generated.TextResult{}, err
	}
	// JSON can expand one text byte to six bytes. Reserve space under the
	// 8 MiB stdio limit for the result envelope and SDK metadata.
	maxBytes := int64(64 << 10)
	if in.MaxBytes != nil {
		if *in.MaxBytes < 1 || *in.MaxBytes > 16<<20 {
			return nil, generated.TextResult{}, errors.New("max_bytes must be between 1 and 16777216")
		}
		maxBytes = min(*in.MaxBytes, 1<<20)
	}
	h = h.withLongRunningClient()
	ctx, cancel := context.WithTimeout(ctx, 36*time.Minute)
	defer cancel()
	result, err := h.options.Client.AttachmentText(ctx, &generated.AttachmentTextRequestOptions{PathParams: &generated.AttachmentTextPath{ProjectID: project.ID, Ref: ref, AttachmentID: in.Attachment}, Query: &generated.AttachmentTextQuery{MaxBytes: &maxBytes, Profile: optionalString(in.Profile)}})
	if err != nil {
		return nil, generated.TextResult{}, err
	}
	return successResult(), *result, nil
}
