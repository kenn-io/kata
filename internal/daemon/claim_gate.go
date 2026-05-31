package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
)

func requireFederatedIssueClaim(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	issue db.Issue,
	actor string,
) error {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled {
		return nil
	}
	if binding.Role == db.FederationRoleSpoke && !binding.PushEnabled {
		return federationReadOnlyError(db.ErrFederatedReadOnly)
	}

	principal := db.ClaimPrincipal{
		HolderInstanceUID: cfg.DB.InstanceUID(),
		Holder:            strings.TrimSpace(actor),
		ClientKind:        "",
	}

	if binding.Role == db.FederationRoleSpoke {
		if err := refreshSpokeClaimStatusForGate(ctx, cfg, binding, issue); err != nil {
			return err
		}
	}

	err = cfg.DB.CheckClaimGate(ctx, db.ClaimGateParams{
		ProjectID: projectID,
		IssueRef:  issue.UID,
		Principal: principal,
		Now:       time.Now().UTC(),
	})
	if err != nil {
		return claimGateAPIError(err)
	}
	return nil
}

func requireFederatedHubIssueClaim(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	issue db.Issue,
	actor string,
) error {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil
	}
	if err != nil {
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleHub {
		return nil
	}
	return requireFederatedIssueClaim(ctx, cfg, projectID, issue, actor)
}

func refreshSpokeClaimStatusForGate(
	ctx context.Context,
	cfg ServerConfig,
	binding db.FederationBinding,
	issue db.Issue,
) error {
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		if isOfflineClaimRefreshError(err) {
			return nil
		}
		return err
	}
	resp, err := remote.ClaimStatus(ctx, cred.HubProjectID, issue.ShortID)
	if err != nil {
		if isTransportClaimError(err) {
			return nil
		}
		return claimForwardError(err)
	}
	if err := cfg.DB.ApplyClaimStatus(ctx, binding.ProjectID, issue.UID, claimStatusFromAPI(resp)); err != nil {
		return claimAPIError(err)
	}
	return nil
}

func isOfflineClaimRefreshError(err error) bool {
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr == nil {
		return false
	}
	return apiErr.Status == http.StatusServiceUnavailable &&
		apiErr.Code == "federation_offline"
}

func claimGateAPIError(err error) error {
	switch {
	case errors.Is(err, db.ErrClaimDenied):
		return api.NewError(http.StatusConflict, "claim_denied",
			"lease denied for federated issue mutation", "run kata federation lease acquire <ref>", nil)
	case errors.Is(err, db.ErrClaimExpired):
		return api.NewError(http.StatusConflict, "claim_expired",
			"lease expired for federated issue mutation", "run kata federation lease acquire <ref>", nil)
	case errors.Is(err, db.ErrClaimRequired), errors.Is(err, db.ErrPendingClaimNotAuthoritative):
		return api.NewError(http.StatusConflict, "claim_required",
			"lease is not authoritative for this federated issue mutation", "run kata show <ref>", nil)
	case errors.Is(err, db.ErrClaimValidation):
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(http.StatusNotFound, "issue_not_found", "issue not found", "", nil)
	default:
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
}
