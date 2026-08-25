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

type federationAuthorization struct {
	principal        federationPrincipal
	transactionFence db.TransactionFence
}

type federationAuthorizationContextKey struct{}

type cachedFederationAuthorization struct {
	authHeader    string
	projectID     int64
	capability    string
	operation     HostFederationOperation
	authorization federationAuthorization
}

func authorizeFederationRequest(
	ctx context.Context,
	cfg ServerConfig,
	authHeader string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
) (context.Context, federationPrincipal, error) {
	authorization, ok := federationAuthorizationFromContext(
		ctx, authHeader, projectID, capability, operation,
	)
	if !ok {
		var err error
		authorization, err = evaluateFederationRequest(
			ctx, cfg, authHeader, projectID, capability, operation,
		)
		if err != nil {
			return ctx, federationPrincipal{}, err
		}
	}
	return db.WithAdditionalTransactionFence(ctx, authorization.transactionFence),
		authorization.principal, nil
}

func withFederationAuthorization(
	ctx context.Context,
	authHeader string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
	authorization federationAuthorization,
) context.Context {
	return context.WithValue(ctx, federationAuthorizationContextKey{}, cachedFederationAuthorization{
		authHeader: authHeader, projectID: projectID, capability: capability,
		operation: operation, authorization: authorization,
	})
}

func federationAuthorizationFromContext(
	ctx context.Context,
	authHeader string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
) (federationAuthorization, bool) {
	cached, ok := ctx.Value(federationAuthorizationContextKey{}).(cachedFederationAuthorization)
	// The whole operation is compared, not just its ID: a cached authorization
	// for a non-mutating call carries no transaction fence, so it must never be
	// reused by a mutating call for the same route.
	if !ok || cached.authHeader != authHeader || cached.projectID != projectID ||
		cached.capability != capability || cached.operation != operation {
		return federationAuthorization{}, false
	}
	return cached.authorization, true
}

func evaluateFederationRequest(
	ctx context.Context,
	cfg ServerConfig,
	authHeader string,
	projectID int64,
	capability string,
	operation HostFederationOperation,
) (federationAuthorization, error) {
	if !strings.HasPrefix(authHeader, authBearerPrefix) {
		return federationAuthorization{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization bearer required", "", nil)
	}
	token := strings.TrimPrefix(authHeader, authBearerPrefix)
	if token == "" {
		return federationAuthorization{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization bearer required", "", nil)
	}

	enrollment, err := cfg.DB.AuthorizeFederationToken(ctx, token, projectID, capability)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return federationAuthorization{}, api.NewError(http.StatusForbidden, "auth_invalid",
				"federation token is invalid for this project or capability", "", nil)
		}
		return federationAuthorization{}, internalAPIError(err)
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return federationAuthorization{}, err
	}
	var transactionFence db.TransactionFence
	if operation.Mutation {
		transactionFence = sanitizeNativeFederationTransactionFence(
			cfg.DB.FederationEnrollmentTransactionFence(enrollment, projectID, capability),
		)
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
			return federationAuthorization{}, federationCredentialDenied()
		}
		if errors.Is(accessErr, ErrHostFederationAdmissionLimited) {
			return federationAuthorization{}, api.NewError(http.StatusTooManyRequests,
				"admission_limited", "federation request admission is full", "", nil)
		}
		if accessErr != nil {
			return federationAuthorization{}, api.NewError(http.StatusServiceUnavailable,
				"access_unavailable", "federation credential authorization is unavailable", "", nil)
		}
		if operation.Mutation && decision.TransactionFence == nil {
			return federationAuthorization{}, api.NewError(http.StatusServiceUnavailable,
				"access_unavailable", "federation transaction access decision unavailable", "", nil)
		}
		transactionFence = composeFederationTransactionFences(
			transactionFence,
			sanitizeFederationTransactionFence(decision.TransactionFence),
		)
	}
	return federationAuthorization{
		principal: federationPrincipal{
			EnrollmentID:                 enrollment.ID,
			SpokeInstanceUID:             enrollment.SpokeInstanceUID,
			Capabilities:                 enrollment.Capabilities,
			Actor:                        enrollment.Actor,
			AllowAdoptionSnapshotAuthors: enrollment.AllowAdoptionSnapshotAuthors,
			AllowAdoptionBaseline:        enrollment.AllowAdoptionSnapshotAuthors || enrollment.AdoptionBaselineOpen,
		},
		transactionFence: transactionFence,
	}, nil
}

func sanitizeNativeFederationTransactionFence(fence db.TransactionFence) db.TransactionFence {
	return func(ctx context.Context, transaction db.Transaction) error {
		err := fence(ctx, transaction)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, db.ErrNotFound):
			return ErrHostAccessDenied
		default:
			return errHostFederationAccessUnavailable
		}
	}
}

func composeFederationTransactionFences(first, second db.TransactionFence) db.TransactionFence {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return func(ctx context.Context, transaction db.Transaction) error {
		if err := first(ctx, transaction); err != nil {
			return err
		}
		return second(ctx, transaction)
	}
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
