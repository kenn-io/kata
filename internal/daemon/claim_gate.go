package daemon

import (
	"context"
	"encoding/json"
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

	holder := strings.TrimSpace(actor)
	if binding.Role == db.FederationRoleSpoke && binding.PushEnabled {
		if bound := strings.TrimSpace(binding.Actor); bound != "" {
			holder = bound
		}
	}
	principal := db.ClaimPrincipal{
		HolderInstanceUID: cfg.DB.InstanceUID(),
		Holder:            holder,
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
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
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
		pending, pendingErr := isPendingSpokePushClaimStatusMiss(ctx, cfg, binding, issue, err)
		if pendingErr != nil {
			return pendingErr
		}
		if pending {
			return nil
		}
		return claimForwardError(err)
	}
	if err := cfg.DB.ApplyClaimStatus(ctx, binding.ProjectID, issue.UID, claimStatusFromAPI(resp)); err != nil {
		return claimAPIError(err)
	}
	return nil
}

func isPendingSpokePushClaimStatusMiss(
	ctx context.Context,
	cfg ServerConfig,
	binding db.FederationBinding,
	issue db.Issue,
	err error,
) (bool, error) {
	var statusErr *claimHubStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		return false, nil
	}
	if hubStatusErrorCode(statusErr) != "issue_not_found" {
		return false, nil
	}
	if binding.Role != db.FederationRoleSpoke || !binding.PushEnabled {
		return false, nil
	}
	pending, err := pendingPushMayMaterializeIssue(ctx, cfg.DB, binding, issue.UID)
	if err != nil {
		return false, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	return pending, nil
}

func hubStatusErrorCode(err *claimHubStatusError) string {
	if err == nil {
		return ""
	}
	var env api.ErrorEnvelope
	if json.Unmarshal([]byte(err.Body), &env) != nil {
		return ""
	}
	return strings.TrimSpace(env.Error.Code)
}

func pendingPushMayMaterializeIssue(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
	issueUID string,
) (bool, error) {
	afterID := binding.PushCursorEventID
	for {
		events, err := store.PendingFederationPushEvents(ctx, binding.ProjectID, store.InstanceUID(), afterID, 1000)
		if err != nil {
			return false, err
		}
		if len(events) == 0 {
			return false, nil
		}
		for _, ev := range events {
			if pushEventMaterializesIssue(ev, issueUID) {
				return true, nil
			}
		}
		nextAfterID := events[len(events)-1].ID
		if nextAfterID <= afterID {
			return false, nil
		}
		afterID = nextAfterID
	}
}

func pushEventMaterializesIssue(ev db.Event, issueUID string) bool {
	if ev.Type != "issue.created" && ev.Type != "issue.snapshot" {
		return false
	}
	return ev.IssueUID != nil && *ev.IssueUID == issueUID
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
