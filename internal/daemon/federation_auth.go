package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type federationPrincipal struct {
	EnrollmentID                 int64
	SpokeInstanceUID             string
	Capabilities                 string
	Actor                        string
	AllowAdoptionSnapshotAuthors bool
	AllowAdoptionBaseline        bool
}

func authorizeFederationRequest(
	ctx context.Context,
	cfg ServerConfig,
	authHeader string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
) (context.Context, federationPrincipal, error) {
	if principal, ok := federationPreauthorizationFromContext(
		ctx, authHeader, projectID, capability, operation,
	); ok {
		return ctx, principal, nil
	}
	if !strings.HasPrefix(authHeader, authBearerPrefix) {
		return ctx, federationPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization bearer required", "", nil)
	}
	token := strings.TrimPrefix(authHeader, authBearerPrefix)
	if token == "" {
		return ctx, federationPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization bearer required", "", nil)
	}

	enrollment, err := cfg.DB.AuthorizeFederationToken(ctx, token, projectID, capability)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ctx, federationPrincipal{}, api.NewError(http.StatusForbidden, "auth_invalid",
				"federation token is invalid for this project or capability", "", nil)
		}
		return ctx, federationPrincipal{}, internalAPIError(err)
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return ctx, federationPrincipal{}, err
	}
	if cfg.HostFederationAccess != nil {
		decision, accessErr := cfg.HostFederationAccess.AuthorizeFederation(
			ctx,
			HostFederationAccessRequest{
				Enrollment: enrollment,
				Project:    project,
				Capability: capability,
				Operation:  operation,
			},
		)
		if errors.Is(accessErr, ErrHostAccessDenied) {
			return ctx, federationPrincipal{}, federationCredentialDenied()
		}
		if accessErr != nil {
			return ctx, federationPrincipal{}, api.NewError(http.StatusServiceUnavailable,
				"access_unavailable", "federation credential authorization is unavailable", "", nil)
		}
		if operation.Mutation && decision.TransactionFence == nil {
			return ctx, federationPrincipal{}, api.NewError(http.StatusServiceUnavailable,
				"access_unavailable", "federation transaction access decision unavailable", "", nil)
		}
		ctx = db.WithAdditionalTransactionFence(ctx, sanitizeFederationTransactionFence(decision.TransactionFence))
	}
	return ctx, federationPrincipal{
		EnrollmentID:                 enrollment.ID,
		SpokeInstanceUID:             enrollment.SpokeInstanceUID,
		Capabilities:                 enrollment.Capabilities,
		Actor:                        enrollment.Actor,
		AllowAdoptionSnapshotAuthors: enrollment.AllowAdoptionSnapshotAuthors,
		AllowAdoptionBaseline:        enrollment.AllowAdoptionSnapshotAuthors || enrollment.AdoptionBaselineOpen,
	}, nil
}

func sanitizeFederationTransactionFence(fence db.TransactionFence) db.TransactionFence {
	if fence == nil {
		return nil
	}
	return func(ctx context.Context, transaction db.Transaction) error {
		err := fence(ctx, transaction)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrHostAccessDenied):
			return ErrHostAccessDenied
		default:
			return errHostFederationAccessUnavailable
		}
	}
}

func federationCredentialDenied() error {
	return api.NewError(http.StatusForbidden, "auth_invalid",
		"federation credential is not currently authorized", "", nil)
}
