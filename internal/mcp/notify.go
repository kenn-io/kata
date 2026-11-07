package mcpserver

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/teammate"
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
	if input.Clear && (input.Message != nil || input.Re != "" || input.Broadcast || input.Teammates) {
		return nil, NotifyOutput{}, errors.New("clear cannot be combined with message, re, broadcast, or teammates")
	}
	if input.Broadcast && input.To != "" {
		return nil, NotifyOutput{}, errors.New("broadcast and to are mutually exclusive")
	}
	if input.Teammates && !input.Broadcast {
		return nil, NotifyOutput{}, errors.New("teammates requires broadcast")
	}
	to := ""
	if !input.Broadcast {
		var err error
		to, err = notification.NormalizeRecipient(input.To)
		if err != nil {
			return nil, NotifyOutput{}, err
		}
	}
	handle := ""
	if !input.Clear {
		if input.Message == nil || strings.TrimSpace(*input.Message) == "" || !utf8.ValidString(*input.Message) || len(*input.Message) > notification.MessageMaxBytes {
			return nil, NotifyOutput{}, errors.New("message must be nonblank valid UTF-8 and at most 1024 bytes")
		}
		var err error
		handle, err = teammate.Resolve(input.Teammate, h.options.Teammate)
		if err != nil {
			return nil, NotifyOutput{}, err
		}
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
			Actor: &h.options.Actor, To: optionalString(to), Message: *input.Message,
			Re: optionalString(input.Re), Teammate: optionalString(handle),
			Broadcast: optionalTrue(input.Broadcast), Teammates: optionalTrue(input.Teammates),
		},
	})
	if err != nil {
		return nil, NotifyOutput{}, err
	}
	result, mutation, err := h.mutationResult(ctx, project, response.Issue, response.Changed, nil, response.Event)
	return result, NotifyOutput{MutationOutput: mutation, Recipients: response.Recipients}, err
}
