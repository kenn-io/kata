package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	oapiruntime "github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/tokenfile"
	"go.kenn.io/kata/internal/tokenissuance"
	"go.kenn.io/kata/pkg/client/generated"
)

// TokensInput requests redacted token records.
type TokensInput struct{}

// TokenSummary is a secret-free token record.
type TokenSummary struct {
	ID         int64              `json:"id"`
	Actor      string             `json:"actor"`
	Name       string             `json:"name,omitempty"`
	Scope      *TokenScopeSummary `json:"scope,omitempty"`
	ExpiresAt  string             `json:"expires_at,omitempty"`
	CreatedAt  string             `json:"created_at"`
	LastUsedAt string             `json:"last_used_at,omitempty"`
	RevokedAt  string             `json:"revoked_at,omitempty"`
}

// TokenScopeSummary is the immutable, secret-free issue grant on a token.
type TokenScopeSummary struct {
	Kind         string `json:"kind"`
	ProjectUID   string `json:"project_uid"`
	RootIssueUID string `json:"root_issue_uid"`
}

// TokensOutput contains redacted token records.
type TokensOutput struct {
	Tokens []TokenSummary `json:"tokens"`
}

// TokenCreateInput identifies the actor subject for a new token.
type TokenCreateInput struct {
	TokenActor       string `json:"token_actor"`
	Name             string `json:"name,omitempty"`
	Issue            string `json:"issue,omitempty"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
	TokenFile        string `json:"token_file,omitempty"`
}

// TokenCreateOutput returns an unscoped token's one-time plaintext or a scoped
// token's protected delivery path, together with its secret-free record.
type TokenCreateOutput struct {
	Token     string       `json:"token,omitempty"`
	TokenFile string       `json:"token_file,omitempty"`
	Issue     string       `json:"issue,omitempty"`
	Record    TokenSummary `json:"record"`
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
	addTool(server, "kata.token_create", "Create token", "Create a daemon token. Scoped tokens are written to a protected token_file and never returned as plaintext.", nonIdempotent(additive), handlers.tokenCreate)
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
	scoped := strings.TrimSpace(input.Issue) != "" || input.ExpiresInSeconds != 0 || strings.TrimSpace(input.TokenFile) != ""
	if scoped {
		return h.scopedTokenCreate(ctx, input, actor)
	}
	response, err := h.options.Client.CreateToken(ctx, &generated.CreateTokenRequestOptions{Body: &generated.CreateTokenBody{Actor: actor, Name: optionalString(input.Name)}})
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	return successResult(), TokenCreateOutput{Token: response.Plaintext, Record: tokenSummary(response.Token)}, nil
}

func (h toolHandlers) scopedTokenCreate(ctx context.Context, input TokenCreateInput, actor string) (_ *sdkmcp.CallToolResult, _ TokenCreateOutput, retErr error) {
	issue := strings.TrimSpace(input.Issue)
	tokenPath := strings.TrimSpace(input.TokenFile)
	if issue == "" || tokenPath == "" || input.ExpiresInSeconds <= 0 {
		return nil, TokenCreateOutput{}, errors.New("scoped token creation requires issue, positive expires_in_seconds, and token_file together")
	}
	if input.ExpiresInSeconds > math.MaxInt64/int64(time.Second) {
		return nil, TokenCreateOutput{}, errors.New("expires_in_seconds is too large")
	}
	instance, err := h.options.Client.Instance(ctx)
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	if !instance.IssueSubtreeTokens {
		return nil, TokenCreateOutput{}, errors.New("the selected daemon does not support issue-scoped tokens")
	}
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, issue, false)
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
	})
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	if project.UID == "" || shown.Issue.UID == "" || shown.Issue.ShortID == "" || shown.Issue.ProjectID != project.ID {
		return nil, TokenCreateOutput{}, errors.New("daemon returned incomplete issue identity for scoped token creation")
	}
	reservation, err := tokenfile.Reserve(tokenPath)
	if err != nil {
		return nil, TokenCreateOutput{}, err
	}
	defer func() { retErr = errors.Join(retErr, reservation.Abort()) }()

	duration := time.Duration(input.ExpiresInSeconds) * time.Second
	scope := generated.TokenScopeIn{
		Kind: generated.IssueSubtree, ProjectUID: project.UID, RootIssueUID: shown.Issue.UID,
	}
	response, err := h.options.Client.CreateToken(ctx, &generated.CreateTokenRequestOptions{
		Body: &generated.CreateTokenBody{
			Actor: actor, Name: optionalString(input.Name), Scope: &scope,
			ExpiresInSeconds: &input.ExpiresInSeconds,
		},
	})
	if err != nil {
		if _, ok := errors.AsType[*oapiruntime.ClientAPIError](err); !ok {
			return nil, TokenCreateOutput{}, fmt.Errorf("scoped token creation response was lost; outcome is ambiguous: %w", err)
		}
		return nil, TokenCreateOutput{}, err
	}
	responseScope := (*tokenissuance.Scope)(nil)
	if response.Token.Scope != nil {
		responseScope = &tokenissuance.Scope{
			Kind: string(response.Token.Scope.Kind), ProjectUID: response.Token.Scope.ProjectUID,
			RootIssueUID: response.Token.Scope.RootIssueUID,
		}
	}
	if err := tokenissuance.Validate(tokenissuance.Response{
		ID: response.Token.ID, Actor: response.Token.Actor, Scope: responseScope,
		CreatedAt: response.Token.CreatedAt, ExpiresAt: response.Token.ExpiresAt,
		Plaintext: response.Plaintext,
	}, actor, tokenissuance.Scope{
		Kind: string(scope.Kind), ProjectUID: scope.ProjectUID, RootIssueUID: scope.RootIssueUID,
	}, duration); err != nil {
		return nil, TokenCreateOutput{}, h.cleanupScopedToken(response.Token.ID, err)
	}
	if err := reservation.Commit(response.Plaintext); err != nil {
		return nil, TokenCreateOutput{}, h.cleanupScopedToken(response.Token.ID, err)
	}
	return successResult(), TokenCreateOutput{
		TokenFile: reservation.Path(), Issue: project.Name + "#" + shown.Issue.ShortID,
		Record: tokenSummary(response.Token),
	}, nil
}

func (h toolHandlers) cleanupScopedToken(tokenID int64, cause error) error {
	if tokenID <= 0 {
		return fmt.Errorf("scoped token delivery failed; creation outcome is ambiguous: %w", cause)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := h.options.Client.RevokeToken(cleanupCtx, &generated.RevokeTokenRequestOptions{
		PathParams: &generated.RevokeTokenPath{ID: tokenID},
	})
	if err != nil {
		return fmt.Errorf("scoped token delivery failed and token %d could not be revoked: %v: %w", tokenID, err, cause)
	}
	return fmt.Errorf("scoped token delivery failed; token %d was revoked: %w", tokenID, cause)
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
	if token.ExpiresAt != nil {
		result.ExpiresAt = formatTime(*token.ExpiresAt)
	}
	if token.Scope != nil {
		result.Scope = &TokenScopeSummary{
			Kind: string(token.Scope.Kind), ProjectUID: token.Scope.ProjectUID,
			RootIssueUID: token.Scope.RootIssueUID,
		}
	}
	return result
}
