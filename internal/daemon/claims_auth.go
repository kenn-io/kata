package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type claimPrincipal struct {
	db.ClaimPrincipal
	Local         bool
	IdentityToken bool
}

func resolveClaimPrincipal(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	authz string,
	body api.ClaimActionBody,
	allowEnrollment bool,
	requireMutationLocal bool,
) (claimPrincipal, error) {
	if cfg.Auth.Token != "" {
		if principal, ok, err := resolveLocalClaimPrincipal(ctx, cfg, authz, body, false); ok || err != nil {
			return principal, err
		}
		if allowEnrollment && hasBearerHeader(authz) {
			return resolveEnrollmentClaimPrincipal(ctx, cfg, projectID, authz, body)
		}
		return claimPrincipal{}, localAuthError(cfg, authz)
	}

	if allowEnrollment && hasBearerHeader(authz) {
		principal, err := resolveEnrollmentClaimPrincipal(ctx, cfg, projectID, authz, body)
		if err == nil {
			return principal, nil
		}
		if cfg.InsecureReadonly {
			return claimPrincipal{}, err
		}
	}

	if requireMutationLocal && cfg.InsecureReadonly {
		return claimPrincipal{}, localAuthError(cfg, authz)
	}
	return localClaimPrincipal(cfg, body), nil
}

func resolveEnrollmentClaimPrincipal(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	authz string,
	body api.ClaimActionBody,
) (claimPrincipal, error) {
	fed, err := authorizeFederationRequest(ctx, cfg.DB, authz, projectID, "claim")
	if err != nil {
		return claimPrincipal{}, err
	}
	return claimPrincipal{ClaimPrincipal: db.ClaimPrincipal{
		HolderInstanceUID: fed.SpokeInstanceUID,
		Holder:            fed.Actor,
		ClientKind:        strings.TrimSpace(body.ClientKind),
	}}, nil
}

func authorizeClaimStatusRead(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	authz string,
) error {
	if cfg.Auth.Token == "" {
		if cfg.InsecureReadonly {
			if !hasBearerHeader(authz) {
				return localAuthError(cfg, authz)
			}
			_, err := authorizeFederationRequest(ctx, cfg.DB, authz, projectID, "claim")
			return err
		}
		return nil
	}
	if _, ok, err := resolveLocalClaimPrincipal(ctx, cfg, authz, api.ClaimActionBody{}, true); ok || err != nil {
		return err
	}
	_, err := authorizeFederationRequest(ctx, cfg.DB, authz, projectID, "claim")
	return err
}

func resolveLocalClaimPrincipal(
	ctx context.Context,
	cfg ServerConfig,
	authz string,
	body api.ClaimActionBody,
	allowBootstrap bool,
) (claimPrincipal, bool, error) {
	switch {
	case cfg.Auth.Token == "":
		if cfg.InsecureReadonly {
			return claimPrincipal{}, false, nil
		}
		return localClaimPrincipal(cfg, body), true, nil
	case validLocalBearer(cfg.Auth.Token, authz):
		if cfg.Auth.RequireTokenIdentity && !allowBootstrap {
			return claimPrincipal{}, false, api.NewError(http.StatusForbidden, "bootstrap_token_write_forbidden",
				"bootstrap token cannot perform attributed writes; use a user token", "", nil)
		}
		return localClaimPrincipal(cfg, body), true, nil
	case cfg.Auth.RequireTokenIdentity && hasBearerHeader(authz):
		if cfg.DB == nil {
			return claimPrincipal{}, false, api.NewError(http.StatusInternalServerError, "internal",
				"token identity requires a database", "", nil)
		}
		presented := strings.TrimPrefix(authz, authBearerPrefix)
		token, err := cfg.DB.ResolveAPIToken(ctx, presented)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return claimPrincipal{}, false, nil
			}
			return claimPrincipal{}, false, api.NewError(http.StatusInternalServerError, "internal",
				"token identity lookup failed", "", nil)
		}
		principal := localClaimPrincipalWithHolder(cfg, body, token.Actor)
		principal.IdentityToken = true
		return principal, true, nil
	default:
		return claimPrincipal{}, false, nil
	}
}

func localClaimPrincipal(cfg ServerConfig, body api.ClaimActionBody) claimPrincipal {
	return localClaimPrincipalWithHolder(cfg, body, body.Holder)
}

func localClaimPrincipalWithHolder(cfg ServerConfig, body api.ClaimActionBody, holder string) claimPrincipal {
	return claimPrincipal{Local: true, ClaimPrincipal: db.ClaimPrincipal{
		HolderInstanceUID: cfg.DB.InstanceUID(),
		Holder:            strings.TrimSpace(holder),
		ClientKind:        strings.TrimSpace(body.ClientKind),
	}}
}

func hasBearerHeader(authz string) bool {
	return strings.HasPrefix(authz, authBearerPrefix)
}

func validLocalBearer(token, authz string) bool {
	if token == "" || !strings.HasPrefix(authz, authBearerPrefix) {
		return false
	}
	presented := strings.TrimPrefix(authz, authBearerPrefix)
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

func localAuthError(cfg ServerConfig, authz string) error {
	if cfg.Auth.Token == "" {
		return api.NewError(http.StatusUnauthorized, "auth_required",
			"mutations and event stream require authentication; daemon is in --insecure-readonly mode", "", nil)
	}
	if !strings.HasPrefix(authz, authBearerPrefix) {
		return api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization bearer required", "", nil)
	}
	return api.NewError(http.StatusForbidden, "auth_invalid", "token mismatch", "", nil)
}
