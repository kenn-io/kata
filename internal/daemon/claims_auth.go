package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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

const hostClaimClientKind = "host:v1"

func resolveClaimPrincipal(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	authz string,
	body api.ClaimActionBody,
	operation HostFederationOperation,
	allowEnrollment bool,
	requireMutationLocal bool,
) (context.Context, claimPrincipal, error) {
	if cfg.HostAccess != nil {
		if requestPrincipal, ok := PrincipalFromContext(ctx); ok {
			if !validHostPrincipal(requestPrincipal) {
				return ctx, claimPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
					"valid host principal required", "", nil)
			}
			return ctx, hostClaimPrincipal(cfg, body, requestPrincipal.Subject), nil
		}
		if allowEnrollment && hasBearerHeader(authz) {
			return resolveEnrollmentClaimPrincipal(ctx, cfg, projectID, authz, body, operation)
		}
		return ctx, claimPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"host principal or scoped federation bearer required", "", nil)
	}
	if cfg.Auth.Token != "" {
		if principal, ok, err := resolveLocalClaimPrincipal(ctx, cfg, authz, body, false); ok || err != nil {
			return ctx, principal, err
		}
		if allowEnrollment && hasBearerHeader(authz) {
			return resolveEnrollmentClaimPrincipal(ctx, cfg, projectID, authz, body, operation)
		}
		return ctx, claimPrincipal{}, localAuthError(cfg, authz)
	}

	if allowEnrollment && hasBearerHeader(authz) {
		authorizedCtx, principal, err := resolveEnrollmentClaimPrincipal(
			ctx, cfg, projectID, authz, body, operation,
		)
		if err == nil {
			return authorizedCtx, principal, nil
		}
		if cfg.InsecureReadonly || cfg.HostFederationAccess != nil {
			return ctx, claimPrincipal{}, err
		}
	}

	if requireMutationLocal && cfg.InsecureReadonly {
		return ctx, claimPrincipal{}, localAuthError(cfg, authz)
	}
	return ctx, localClaimPrincipal(cfg, body), nil
}

func hostClaimPrincipal(cfg ServerConfig, body api.ClaimActionBody, subject string) claimPrincipal {
	principal := localClaimPrincipalWithHolder(cfg, body, hostClaimHolder(subject))
	principal.ClientKind = hostClaimClientKind
	principal.AuthenticatedHost = true
	return principal
}

func hostClaimHolder(subject string) string {
	digest := sha256.Sum256([]byte("kata:host-claim-holder:v1\x00" + subject))
	return "host:" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func resolveEnrollmentClaimPrincipal(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	authz string,
	body api.ClaimActionBody,
	operation HostFederationOperation,
) (context.Context, claimPrincipal, error) {
	authorizedCtx, fed, err := authorizeFederationRequest(ctx, cfg, authz, projectID, "claim",
		operation)
	if err != nil {
		return ctx, claimPrincipal{}, err
	}
	return authorizedCtx, claimPrincipal{ClaimPrincipal: db.ClaimPrincipal{
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
) (context.Context, error) {
	if cfg.HostAccess != nil {
		if requestPrincipal, ok := PrincipalFromContext(ctx); ok {
			if validHostPrincipal(requestPrincipal) {
				return ctx, nil
			}
			return ctx, api.NewError(http.StatusUnauthorized, "auth_required",
				"valid host principal required", "", nil)
		}
		if hasBearerHeader(authz) {
			authorizedCtx, _, err := authorizeFederationRequest(ctx, cfg, authz, projectID, "claim",
				HostFederationOperation{ID: "getIssueLeaseStatus", Mutation: true})
			return authorizedCtx, err
		}
		return ctx, api.NewError(http.StatusUnauthorized, "auth_required",
			"host principal or scoped federation bearer required", "", nil)
	}
	if cfg.Auth.Token == "" {
		if hasBearerHeader(authz) {
			authorizedCtx, _, err := authorizeFederationRequest(ctx, cfg, authz, projectID, "claim",
				HostFederationOperation{ID: "getIssueLeaseStatus", Mutation: true})
			return authorizedCtx, err
		}
		if cfg.InsecureReadonly {
			return ctx, localAuthError(cfg, authz)
		}
		return ctx, nil
	}
	if _, ok, err := resolveLocalClaimPrincipal(ctx, cfg, authz, api.ClaimActionBody{}, true); ok || err != nil {
		return ctx, err
	}
	authorizedCtx, _, err := authorizeFederationRequest(ctx, cfg, authz, projectID, "claim",
		HostFederationOperation{ID: "getIssueLeaseStatus", Mutation: true})
	return authorizedCtx, err
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
