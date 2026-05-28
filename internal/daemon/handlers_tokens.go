package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

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
		if err := db.ValidateTokenActor(in.Body.Actor); err != nil {
			return nil, api.NewError(400, "validation", err.Error(), "", nil)
		}
		plaintext, err := newPlaintextToken()
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		var name *string
		if trimmed := strings.TrimSpace(in.Body.Name); trimmed != "" {
			name = &trimmed
		}
		tok, _, err := cfg.DB.CreateAPIToken(ctx, db.CreateAPITokenParams{
			PlaintextToken: plaintext,
			Actor:          in.Body.Actor,
			Name:           name,
			AdminActor:     tokenAdminAuditActor(ctx, db.BootstrapActor),
		})
		if err != nil {
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.CreateTokenResponse{}
		out.Body.Token = tokenOut(tok)
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
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.ListTokensResponse{}
		out.Body.Tokens = make([]api.TokenOut, 0, len(toks))
		for _, tok := range toks {
			out.Body.Tokens = append(out.Body.Tokens, tokenOut(tok))
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
			return nil, api.NewError(500, "internal", err.Error(), "", nil)
		}
		out := &api.RevokeTokenResponse{}
		out.Body.Token = tokenOut(tok)
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

func tokenOut(tok db.APIToken) api.TokenOut {
	return api.TokenOut{
		ID:         tok.ID,
		Actor:      tok.Actor,
		Name:       tok.Name,
		CreatedAt:  tok.CreatedAt,
		LastUsedAt: tok.LastUsedAt,
		RevokedAt:  tok.RevokedAt,
	}
}
