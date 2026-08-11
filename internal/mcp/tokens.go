package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// TokensInput requests redacted token records.
type TokensInput struct{}

// TokenSummary is a secret-free token record.
type TokenSummary struct {
	ID         int64  `json:"id"`
	Actor      string `json:"actor"`
	Name       string `json:"name,omitempty"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
}

// TokensOutput contains redacted token records.
type TokensOutput struct {
	Tokens []TokenSummary `json:"tokens"`
}

// TokenCreateInput identifies the actor subject for a new token.
type TokenCreateInput struct {
	TokenActor string `json:"token_actor"`
	Name       string `json:"name,omitempty"`
}

// TokenCreateOutput returns the one-time plaintext and its record.
type TokenCreateOutput struct {
	Token  string       `json:"token"`
	Record TokenSummary `json:"record"`
}

// TokenRevokeInput selects a token record to revoke.
type TokenRevokeInput struct {
	ID int64 `json:"id"`
}

// TokenRevokeOutput reports the revoked token record.
type TokenRevokeOutput struct {
	Record TokenSummary  `json:"record"`
	Event  *EventSummary `json:"event,omitempty"`
}

func registerTokenTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	mutating := toolHints(false, true, false)
	additive := toolHints(false, false, false)
	addTool(server, "kata.token_create", "Create token", "Create a daemon token; the plaintext appears only in this result.", additive, handlers.tokenCreate)
	addTool(server, "kata.token_revoke", "Revoke token", "Revoke one daemon token by ID.", mutating, handlers.tokenRevoke)
	addTool(server, "kata.tokens", "List tokens", "List redacted daemon token records without secrets or hashes.", read, handlers.tokens)
}

func (h toolHandlers) tokens(ctx context.Context, _ *sdkmcp.CallToolRequest, _ TokensInput) (*sdkmcp.CallToolResult, TokensOutput, error) {
	if err := h.requireDaemonWideScope("token administration"); err != nil {
		return nil, TokensOutput{}, err
	}
	response, err := h.options.Client.ListTokens(ctx)
	if err != nil {
		return nil, TokensOutput{}, err
	}
	tokens := make([]TokenSummary, 0, len(response.Tokens))
	for _, token := range response.Tokens {
		tokens = append(tokens, tokenSummary(token))
	}
	return successResult(), TokensOutput{Tokens: tokens}, nil
}

func (h toolHandlers) tokenCreate(ctx context.Context, _ *sdkmcp.CallToolRequest, input TokenCreateInput) (*sdkmcp.CallToolResult, TokenCreateOutput, error) {
	if err := h.requireDaemonWideScope("token administration"); err != nil {
		return nil, TokenCreateOutput{}, err
	}
	actor := strings.TrimSpace(input.TokenActor)
	if actor == "" {
		return nil, TokenCreateOutput{}, errors.New("token_actor is required")
	}
	response, err := h.options.Client.CreateToken(ctx, &generated.CreateTokenRequestOptions{Body: &generated.CreateTokenBody{Actor: actor, Name: optionalString(input.Name)}})
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	return successResult(), TokenCreateOutput{Token: response.Plaintext, Record: tokenSummary(response.Token)}, nil
}

func (h toolHandlers) tokenRevoke(ctx context.Context, _ *sdkmcp.CallToolRequest, input TokenRevokeInput) (*sdkmcp.CallToolResult, TokenRevokeOutput, error) {
	if err := h.requireDaemonWideScope("token administration"); err != nil {
		return nil, TokenRevokeOutput{}, err
	}
	if input.ID <= 0 {
		return nil, TokenRevokeOutput{}, errors.New("id must be positive")
	}
	response, err := h.options.Client.RevokeToken(ctx, &generated.RevokeTokenRequestOptions{PathParams: &generated.RevokeTokenPath{ID: input.ID}})
	if err != nil {
		return nil, TokenRevokeOutput{}, err
	}
	return successResult(), TokenRevokeOutput{Record: tokenSummary(response.Token), Event: eventSummary(&response.Event)}, nil
}

func (h toolHandlers) requireDaemonWideScope(operation string) error {
	if !h.options.EnableTokenAdmin {
		return fmt.Errorf("%s requires an explicit startup capability", operation)
	}
	if h.options.Scope.Mode() != ScopeAll {
		return fmt.Errorf("%s requires daemon-wide scope", operation)
	}
	return nil
}

func tokenSummary(token generated.TokenOut) TokenSummary {
	result := TokenSummary{ID: token.ID, Actor: token.Actor, CreatedAt: formatTime(token.CreatedAt)}
	if token.Name != nil {
		result.Name = *token.Name
	}
	if token.LastUsedAt != nil {
		result.LastUsedAt = formatTime(*token.LastUsedAt)
	}
	if token.RevokedAt != nil {
		result.RevokedAt = formatTime(*token.RevokedAt)
	}
	return result
}
