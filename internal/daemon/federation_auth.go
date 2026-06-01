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
	EnrollmentID     int64
	SpokeInstanceUID string
	Capabilities     string
}

func authorizeFederationRequest(
	ctx context.Context,
	store db.Storage,
	authHeader string,
	projectID int64,
	capability string,
) (federationPrincipal, error) {
	if !strings.HasPrefix(authHeader, authBearerPrefix) {
		return federationPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization: Bearer <token> required", "", nil)
	}
	token := strings.TrimPrefix(authHeader, authBearerPrefix)
	if token == "" {
		return federationPrincipal{}, api.NewError(http.StatusUnauthorized, "auth_required",
			"Authorization: Bearer <token> required", "", nil)
	}

	enrollment, err := store.AuthorizeFederationToken(ctx, token, projectID, capability)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return federationPrincipal{}, api.NewError(http.StatusForbidden, "auth_invalid",
				"federation token is invalid for this project or capability", "", nil)
		}
		return federationPrincipal{}, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	return federationPrincipal{
		EnrollmentID:     enrollment.ID,
		SpokeInstanceUID: enrollment.SpokeInstanceUID,
		Capabilities:     enrollment.Capabilities,
	}, nil
}
