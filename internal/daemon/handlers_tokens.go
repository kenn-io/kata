package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func registerTokenHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "createToken",
		Method:      "POST",
		Path:        "/api/v1/tokens",
	}, func(ctx context.Context, in *api.CreateTokenRequest) (*api.CreateTokenResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		if (cfg.Auth.Token != "" || in.Body.Scope != nil) && !cfg.Auth.RequireTokenIdentity {
			return nil, api.NewError(409, "token_identity_mode_required",
				"identity tokens are not accepted while auth.require_token_identity is false",
				"set auth.require_token_identity = true and restart the daemon before creating identity tokens", nil)
		}
		if err := db.ValidateTokenActor(in.Body.Actor); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		plaintext, err := newPlaintextToken()
		if err != nil {
			return nil, internalAPIError(err)
		}
		var name *string
		if trimmed := strings.TrimSpace(in.Body.Name); trimmed != "" {
			name = &trimmed
		}
		scope, expiresAt, err := validateTokenGrantRequest(ctx, cfg, in.Body.Scope, in.Body.ExpiresInSeconds)
		if err != nil {
			return nil, err
		}
		tok, _, err := cfg.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: plaintext,
			Actor:          in.Body.Actor,
			Name:           name,
			AdminActor:     tokenAdminAuditActor(ctx, db.BootstrapActor),
			Scope:          scope,
			ExpiresAt:      expiresAt,
		})
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.CreateTokenResponse{}
		out.Body.Token = tokenOut(tok, tokenObservationTime(cfg))
		out.Body.Plaintext = plaintext
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "listTokens",
		Method:      "GET",
		Path:        "/api/v1/tokens",
	}, func(ctx context.Context, _ *struct{}) (*api.ListTokensResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		toks, err := cfg.DB.ListAPITokens(ctx)
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.ListTokensResponse{}
		observedAt := tokenObservationTime(cfg)
		out.Body.ObservedAt = observedAt
		out.Body.Tokens = make([]api.TokenOut, 0, len(toks))
		for _, tok := range toks {
			out.Body.Tokens = append(out.Body.Tokens, tokenOut(tok, observedAt))
		}
		return out, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "revokeToken",
		Method:      "POST",
		Path:        "/api/v1/tokens/{id}/actions/revoke",
	}, func(ctx context.Context, in *api.RevokeTokenRequest) (*api.RevokeTokenResponse, error) {
		if err := ensureTokenAdminAllowed(ctx); err != nil {
			return nil, err
		}
		tok, evt, err := cfg.DB.RevokeAPIToken(ctx, in.ID, tokenAdminAuditActor(ctx, db.BootstrapActor))
		if errors.Is(err, db.ErrNotFound) {
			return nil, api.NewError(404, "token_not_found", "token not found", "", nil)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		out := &api.RevokeTokenResponse{}
		out.Body.Token = tokenOut(tok, tokenObservationTime(cfg))
		out.Body.Event = &evt
		return out, nil
	})
}

func newPlaintextToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "kata_" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func tokenOut(tok db.APIToken, observedAt time.Time) api.TokenOut {
	out := api.TokenOut{
		ID:         tok.ID,
		Actor:      tok.Actor,
		Name:       tok.Name,
		CreatedAt:  tok.CreatedAt,
		LastUsedAt: tok.LastUsedAt,
		RevokedAt:  tok.RevokedAt,
		State:      tokenLifecycleState(tok, observedAt),
		ExpiresAt:  tok.ExpiresAt,
	}
	if tok.Scope != nil {
		out.Scope = tokenScopeOut(tok.Scope)
	}
	return out
}

func tokenLifecycleState(tok db.APIToken, observedAt time.Time) string {
	if tok.RevokedAt != nil {
		return "revoked"
	}
	if tok.ExpiresAt != nil && !observedAt.UTC().Before(tok.ExpiresAt.UTC()) {
		return "expired"
	}
	return "live"
}

func tokenObservationTime(cfg ServerConfig) time.Time {
	if cfg.UIClock != nil {
		return cfg.UIClock().UTC()
	}
	return time.Now().UTC()
}

func tokenScopeOut(scope *db.APITokenScope) *api.TokenScopeOut {
	if scope == nil {
		return nil
	}
	return &api.TokenScopeOut{
		Kind: string(scope.Kind), ProjectUID: scope.ProjectUID, RootIssueUID: scope.RootIssueUID,
	}
}

func validateTokenGrantRequest(
	ctx context.Context,
	cfg ServerConfig,
	requested *api.TokenScopeIn,
	expiresInSeconds int64,
) (*db.APITokenScope, *time.Time, error) {
	if requested == nil {
		if expiresInSeconds != 0 {
			return nil, nil, api.NewError(400, "invalid_token_scope",
				"expires_in_seconds requires a token scope", "", nil)
		}
		return nil, nil, nil
	}
	if expiresInSeconds <= 0 || expiresInSeconds > math.MaxInt64/int64(time.Second) {
		return nil, nil, api.NewError(400, "invalid_token_scope",
			"scoped tokens require a positive, non-overflowing expires_in_seconds", "", nil)
	}
	scope := &db.APITokenScope{
		Kind:         db.APITokenScopeKind(requested.Kind),
		ProjectUID:   requested.ProjectUID,
		RootIssueUID: requested.RootIssueUID,
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresInSeconds) * time.Second)
	if err := db.ValidateAPITokenGrant(scope, &expiresAt); err != nil {
		return nil, nil, api.NewError(400, "invalid_token_scope", err.Error(), "", nil)
	}
	project, err := activeProjectByUID(ctx, cfg.DB, scope.ProjectUID)
	if err != nil {
		return nil, nil, api.NewError(400, "invalid_token_scope",
			"token scope root and project must identify an active issue in one active project", "", nil)
	}
	root, err := cfg.DB.IssueByUID(ctx, scope.RootIssueUID, db.IncludeDeletedNo)
	if err != nil || root.ProjectID != project.ID {
		return nil, nil, api.NewError(400, "invalid_token_scope",
			"token scope root and project must identify an active issue in one active project", "", nil)
	}
	if binding, bindingErr := cfg.DB.FederationBindingByProject(ctx, project.ID); bindingErr == nil {
		if binding.Role == db.FederationRoleSpoke {
			return nil, nil, api.NewError(409, "scoped_token_spoke_forbidden",
				"issue-scoped tokens must be created on the project's authoritative hub",
				"target the trusted hub daemon and create the credential there", nil)
		}
	} else if !errors.Is(bindingErr, db.ErrNotFound) {
		return nil, nil, internalAPIError(bindingErr)
	}
	return scope, &expiresAt, nil
}
