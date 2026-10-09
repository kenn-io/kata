package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

type federationPrincipal struct {
	RelayBindingUID              string
	RelayProtocolVersion         int
	RelayServeDownstream         bool
	RelayResetEpoch              int64
	ScopedProjectID              int64
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
	projectUID       string
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
	ctx = withFederationProjectAuthority(ctx, authorization)
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
	ctx = withFederationProjectAuthority(ctx, authorization)
	return context.WithValue(ctx, federationAuthorizationContextKey{}, cachedFederationAuthorization{
		authHeader: authHeader, projectID: projectID, capability: capability,
		operation: operation, authorization: authorization,
	})
}

func withFederationProjectAuthority(ctx context.Context, authorization federationAuthorization) context.Context {
	if decision, _ := ctx.Value(projectAccessContextKey{}).(*ProjectAccessDecision); decision != nil {
		decision.Actor = authorization.principal.Actor
		decision.ProjectUIDs = []string{authorization.projectUID}
		decision.targets = []string{authorization.projectUID}
	}
	return db.WithAuthorizedProjects(ctx, []string{authorization.projectUID})
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
	var project db.Project
	if _, authenticated := PrincipalFromContext(ctx); authenticated {
		project, err = activeProjectByID(ctx, cfg.DB, projectID)
	} else {
		// A native enrollment is itself the authenticated transport principal.
		// The outer project middleware can only supply anonymous scope before
		// this token is validated, so resolve the target first and apply the
		// enrollment actor's project boundary below.
		project, err = cfg.DB.ProjectByID(ctx, projectID)
		if errors.Is(err, db.ErrNotFound) || (err == nil && project.DeletedAt != nil) {
			err = api.NewError(http.StatusNotFound, "project_not_found", "project not found", "", nil)
		} else if err != nil {
			err = internalAPIError(err)
		}
	}
	if err != nil {
		return federationAuthorization{}, err
	}
	accessCtx := ctx
	if _, authenticated := PrincipalFromContext(ctx); !authenticated {
		// The outer HTTP scope for a native federation bearer is anonymous until
		// the enrollment has been validated. Recompute this one membership check
		// from the enrollment actor, then replace the request scope below with
		// that actor's single authorized project.
		accessCtx = db.WithAuthorizedProjects(ctx, nil)
	}
	allowed, err := cfg.DB.AccessibleProjectUIDs(accessCtx, enrollment.Actor)
	if err != nil {
		return federationAuthorization{}, internalAPIError(err)
	}
	if !slices.Contains(allowed, project.UID) {
		return federationAuthorization{}, projectAccessDenied()
	}
	var transactionFence db.TransactionFence
	if operation.Mutation {
		transactionFence = sanitizeNativeFederationTransactionFence(
			cfg.DB.FederationEnrollmentTransactionFence(enrollment, projectID, capability),
		)
		transactionFence = composeFederationTransactionFences(transactionFence, sanitizeNativeFederationTransactionFence(cfg.DB.ProjectAccessTransactionFence(enrollment.Actor, []string{project.UID})))
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
	scopedProjectID := int64(0)
	if enrollment.ProjectID != nil {
		scopedProjectID = *enrollment.ProjectID
	}
	return federationAuthorization{
		projectUID: project.UID,
		principal: federationPrincipal{
			RelayBindingUID:              enrollment.RelayBindingUID,
			RelayProtocolVersion:         enrollment.RelayProtocolVersion,
			RelayServeDownstream:         enrollment.RelayServeDownstream,
			RelayResetEpoch:              enrollment.RelayResetEpoch,
			EnrollmentID:                 enrollment.ID,
			ScopedProjectID:              scopedProjectID,
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
			// Keep the underlying error so a transient database failure
			// (serialization failure, deadlock, lock timeout) stays
			// recognizable to the storage retry loop. Dropping it turns a
			// retryable conflict into a hard 503.
			return errors.Join(errHostFederationAccessUnavailable, err)
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
			return errors.Join(ErrHostAccessDenied, err)
		default:
			return errors.Join(errHostFederationAccessUnavailable, err)
		}
	}
}

func federationCredentialDenied() error {
	return api.NewError(http.StatusForbidden, "auth_invalid",
		"federation credential is not currently authorized", "", nil)
}
