package mcpserver

import (
	"context"
	"errors"
	"strconv"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/pkg/client/generated"
)

// NotifyInput sends or clears a replaceable inbox signal, not work.attention.
type NotifyInput struct {
	Ref       string  `json:"ref"`
	To        string  `json:"to,omitempty" jsonschema:"Exact recipient actor or actor/teammate; required except for broadcast"`
	Message   *string `json:"message,omitempty" jsonschema:"Nonblank request text, at most 1024 UTF-8 bytes; omit when clearing"`
	Clear     bool    `json:"clear,omitzero" jsonschema:"Remove the exact recipient's request after handling it"`
	Re        string  `json:"re,omitempty" jsonschema:"Comment handle or UID to answer; resolved by the daemon"`
	Broadcast bool    `json:"broadcast,omitzero" jsonschema:"Use the daemon's bounded participant audience instead of to; retries are rate limited"`
	Teammates bool    `json:"teammates,omitzero" jsonschema:"Include participant teammate inboxes in a broadcast"`
	Teammate  *string `json:"teammate,omitempty" jsonschema:"Sender teammate; empty suppresses the startup default"`
}

// NotifyOutput retains native mutation evidence and the resolved audience.
type NotifyOutput struct {
	MutationOutput
	Recipients []string `json:"recipients"`
}

func (h toolHandlers) notify(ctx context.Context, _ *sdkmcp.CallToolRequest, input NotifyInput) (*sdkmcp.CallToolResult, NotifyOutput, error) {
	if input.Clear && (input.Broadcast || input.Teammates) {
		return nil, NotifyOutput{}, errors.New("clear cannot be combined with broadcast or teammates")
	}
	to := input.To
	if input.Clear {
		var err error
		to, err = notification.NormalizeRecipient(to)
		if err != nil {
			return nil, NotifyOutput{}, err
		}
	}
	handle := h.options.Teammate
	if input.Teammate != nil {
		handle = *input.Teammate
	}
	message := ""
	if input.Message != nil {
		message = *input.Message
	}
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, NotifyOutput{}, err
	}
	if input.Clear {
		result, mutation, err := h.patchMetadata(ctx, project, ref, map[string]any{notification.MetadataKey(to): nil}, nil)
		return result, NotifyOutput{MutationOutput: mutation, Recipients: []string{to}}, err
	}
	// The typed route resolves comment references and the effective actor inside
	// the writing daemon. Never construct a caller-supplied from/kind payload.
	response, err := h.options.Client.NotifyIssue(ctx, &generated.NotifyIssueRequestOptions{
		PathParams: &generated.NotifyIssuePath{ProjectID: strconv.FormatInt(project.ID, 10), Ref: ref},
		Body: &generated.NotifyIssueBody{
			Actor: &h.options.Actor, To: &to, Message: message,
			Re: &input.Re, Teammate: &handle,
			Broadcast: optionalTrue(input.Broadcast), Teammates: optionalTrue(input.Teammates),
		},
	})
	if err != nil {
		if notificationRouteUnsupported(err) {
			return nil, NotifyOutput{}, errors.New("daemon does not support notifications; update the daemon to use kata.notify")
		}
		return nil, NotifyOutput{}, err
	}
	result, mutation, err := h.mutationResult(ctx, project, response.Issue, response.Changed, nil, response.Event)
	return result, NotifyOutput{MutationOutput: mutation, Recipients: response.Recipients}, err
}
