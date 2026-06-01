package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
)

func registerClaimHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "acquireIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/acquire",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body, true, true)
		if err != nil {
			return nil, err
		}
		body, err := handleClaimAcquire(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "renewIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/renew",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body, true, true)
		if err != nil {
			return nil, err
		}
		body, err := handleClaimRenew(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "releaseIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/release",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body, true, true)
		if err != nil {
			return nil, err
		}
		body, err := handleClaimRelease(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "forceReleaseIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/force_release",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body, false, true)
		if err != nil {
			return nil, err
		}
		if err := requireHubClaimBinding(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		actor := strings.TrimSpace(in.Body.Actor)
		if principal.IdentityToken {
			actor = principal.Holder
		}
		if actor == "" {
			actor = "admin"
		}
		reason := strings.TrimSpace(in.Body.Reason)
		if reason == "" {
			reason = "admin_force_release"
		}
		result, err := cfg.DB.ForceReleaseClaim(ctx, db.ForceReleaseClaimParams{
			ProjectID: in.ProjectID,
			IssueRef:  in.Ref,
			Actor:     actor,
			Reason:    reason,
			Now:       time.Now().UTC(),
		})
		if err != nil {
			if errors.Is(err, db.ErrClaimExpired) {
				emitClaimEvents(cfg, result.Events)
			}
			return nil, claimAPIError(err)
		}
		emitClaimEvents(cfg, result.Events)
		emitClaimEvent(cfg, in.ProjectID, result.Event)
		return &api.ClaimActionResponse{Body: claimResultBody(result)}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getIssueLeaseStatus",
		Method:      http.MethodGet,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease",
	}, func(ctx context.Context, in *api.ClaimStatusRequest) (*api.ClaimStatusResponse, error) {
		if err := authorizeClaimStatusRead(ctx, cfg, in.ProjectID, in.Authorization); err != nil {
			return nil, err
		}
		body, err := handleClaimStatus(ctx, cfg, in.ProjectID, in.Ref)
		if err != nil {
			return nil, err
		}
		return &api.ClaimStatusResponse{Body: body}, nil
	})
}

func handleClaimAcquire(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	ref string,
	body api.ClaimActionBody,
	principal db.ClaimPrincipal,
) (api.ClaimActionResponseBody, error) {
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	if binding.Role == db.FederationRoleHub {
		result, err := cfg.DB.AcquireClaim(ctx, db.AcquireClaimParams{
			ProjectID: projectID,
			IssueRef:  ref,
			Principal: principal,
			ClaimKind: claimKindOrDefault(body.ClaimKind),
			TTL:       ttlDuration(body.TTLSeconds),
			Purpose:   strings.TrimSpace(body.Purpose),
			Now:       time.Now().UTC(),
		})
		if err != nil {
			if errors.Is(err, db.ErrClaimDenied) {
				return claimResultBody(result), nil
			}
			return api.ClaimActionResponseBody{}, claimAPIError(err)
		}
		emitClaimEvents(cfg, result.Events)
		emitClaimEvent(cfg, projectID, result.Event)
		if err := federationFailpoint("after_claim_grant_commit_before_response"); err != nil {
			return api.ClaimActionResponseBody{}, api.NewError(http.StatusInternalServerError, "federation_failpoint", err.Error(), "", nil)
		}
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	resp, err := remote.AcquireClaim(ctx, cred.HubProjectID, ref, forwardedClaimRequest(body, principal))
	if err != nil {
		if isTransportClaimError(err) {
			pending, enqueueErr := cfg.DB.EnqueuePendingClaim(ctx, db.PendingClaimParams{
				ProjectID: projectID,
				IssueRef:  ref,
				Principal: principal,
				ClaimKind: claimKindOrDefault(body.ClaimKind),
				TTL:       ttlDuration(body.TTLSeconds),
				Purpose:   strings.TrimSpace(body.Purpose),
				Now:       time.Now().UTC(),
			})
			if enqueueErr != nil {
				return api.ClaimActionResponseBody{}, claimAPIError(enqueueErr)
			}
			return api.ClaimActionResponseBody{
				Pending:    true,
				RequestUID: pending.RequestUID,
				Holder:     claimPrincipalOut(principal),
			}, nil
		}
		return api.ClaimActionResponseBody{}, claimForwardError(err)
	}
	if err := applyForwardedClaimAction(ctx, cfg.DB, projectID, ref, resp, true); err != nil {
		return api.ClaimActionResponseBody{}, claimAPIError(err)
	}
	return resp, nil
}

func handleClaimRenew(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	ref string,
	body api.ClaimActionBody,
	principal db.ClaimPrincipal,
) (api.ClaimActionResponseBody, error) {
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	if binding.Role == db.FederationRoleHub {
		result, err := cfg.DB.RenewClaim(ctx, db.RenewClaimParams{
			ProjectID: projectID,
			IssueRef:  ref,
			Principal: principal,
			TTL:       ttlDuration(body.TTLSeconds),
			Now:       time.Now().UTC(),
		})
		if err != nil {
			if errors.Is(err, db.ErrClaimExpired) {
				emitClaimEvents(cfg, result.Events)
			}
			return api.ClaimActionResponseBody{}, claimAPIError(err)
		}
		emitClaimEvents(cfg, result.Events)
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	resp, err := remote.RenewClaim(ctx, cred.HubProjectID, ref, forwardedClaimRequest(body, principal))
	if err != nil {
		return api.ClaimActionResponseBody{}, claimForwardError(err)
	}
	if err := applyForwardedClaimAction(ctx, cfg.DB, projectID, ref, resp, true); err != nil {
		return api.ClaimActionResponseBody{}, claimAPIError(err)
	}
	return resp, nil
}

func handleClaimRelease(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	ref string,
	body api.ClaimActionBody,
	principal db.ClaimPrincipal,
) (api.ClaimActionResponseBody, error) {
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	if binding.Role == db.FederationRoleHub {
		result, err := cfg.DB.ReleaseClaim(ctx, db.ReleaseClaimParams{
			ProjectID: projectID,
			IssueRef:  ref,
			Principal: principal,
			Reason:    strings.TrimSpace(body.Reason),
			Now:       time.Now().UTC(),
		})
		if err != nil {
			if errors.Is(err, db.ErrClaimExpired) {
				emitClaimEvents(cfg, result.Events)
			}
			return api.ClaimActionResponseBody{}, claimAPIError(err)
		}
		emitClaimEvents(cfg, result.Events)
		emitClaimEvent(cfg, projectID, result.Event)
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	resp, err := remote.ReleaseClaim(ctx, cred.HubProjectID, ref, forwardedClaimRequest(body, principal))
	if err != nil {
		return api.ClaimActionResponseBody{}, claimForwardError(err)
	}
	if err := applyForwardedClaimAction(ctx, cfg.DB, projectID, ref, resp, false); err != nil {
		return api.ClaimActionResponseBody{}, claimAPIError(err)
	}
	return resp, nil
}

func handleClaimStatus(ctx context.Context, cfg ServerConfig, projectID int64, ref string) (api.ClaimStatusBody, error) {
	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimStatusBody{}, err
	}
	if binding.Role == db.FederationRoleHub {
		status, err := cfg.DB.ClaimStatus(ctx, projectID, ref, time.Now().UTC())
		if err != nil {
			return api.ClaimStatusBody{}, claimAPIError(err)
		}
		emitClaimEvents(cfg, status.Events)
		return claimStatusBody(status), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		return api.ClaimStatusBody{}, err
	}
	resp, err := remote.ClaimStatus(ctx, cred.HubProjectID, ref)
	if err != nil {
		return api.ClaimStatusBody{}, claimForwardError(err)
	}
	issueRef := ref
	lease := resp.Lease
	if lease == nil {
		lease = resp.Claim
	}
	if lease != nil && lease.IssueUID != "" {
		issueRef = lease.IssueUID
	}
	if err := cfg.DB.ApplyClaimStatus(ctx, projectID, issueRef, claimStatusFromAPI(resp)); err != nil {
		return api.ClaimStatusBody{}, claimAPIError(err)
	}
	return resp, nil
}

func claimBinding(ctx context.Context, store db.Storage, projectID int64) (db.FederationBinding, error) {
	if _, err := activeProjectByID(ctx, store, projectID); err != nil {
		return db.FederationBinding{}, err
	}
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return db.FederationBinding{}, api.NewError(http.StatusNotFound, "federation_not_found", "project is not federated", "", nil)
	}
	if err != nil {
		return db.FederationBinding{}, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled {
		return db.FederationBinding{}, api.NewError(http.StatusConflict, "federated_read_only", "federation binding is disabled", "", nil)
	}
	if binding.Role != db.FederationRoleHub && binding.Role != db.FederationRoleSpoke {
		return db.FederationBinding{}, api.NewError(http.StatusConflict, "federated_read_only", "unknown federation binding role", "", nil)
	}
	return binding, nil
}

func claimForwardClient(
	ctx context.Context,
	store db.Storage,
	binding db.FederationBinding,
) (*claimHubClient, config.FederationCredential, error) {
	project, err := store.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return nil, config.FederationCredential{}, claimAPIError(err)
	}
	creds, err := config.ReadFederationCredentials()
	if err != nil {
		return nil, config.FederationCredential{}, api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	cred := creds.Projects[project.UID]
	if strings.TrimSpace(cred.Token) == "" {
		return nil, config.FederationCredential{}, api.NewError(http.StatusServiceUnavailable, "federation_offline", "federation claim credentials are unavailable", "", nil)
	}
	if cred.HubURL == "" {
		cred.HubURL = binding.HubURL
	}
	if cred.HubProjectID == 0 {
		cred.HubProjectID = binding.HubProjectID
	}
	client, err := newClaimHubClient(ctx, cred.HubURL, cred.Token)
	if err != nil {
		return nil, config.FederationCredential{}, api.NewError(http.StatusServiceUnavailable, "federation_offline", err.Error(), "", nil)
	}
	return client, cred, nil
}

func forwardedClaimRequest(body api.ClaimActionBody, principal db.ClaimPrincipal) api.ClaimActionBody {
	body.Holder = principal.Holder
	body.ClientKind = principal.ClientKind
	body.ClaimKind = claimKindOrDefault(body.ClaimKind)
	body.Purpose = strings.TrimSpace(body.Purpose)
	body.Reason = strings.TrimSpace(body.Reason)
	return body
}

func applyForwardedClaimAction(
	ctx context.Context,
	store db.Storage,
	projectID int64,
	ref string,
	resp api.ClaimActionResponseBody,
	held bool,
) error {
	lease := resp.Lease
	if lease == nil {
		lease = resp.Claim
	}
	issueRef := ref
	if lease != nil && lease.IssueUID != "" {
		issueRef = lease.IssueUID
	}
	return store.ApplyClaimStatus(ctx, projectID, issueRef, db.ClaimStatus{
		Held:   held && lease != nil,
		Holder: claimPrincipalFromAPI(resp.Holder),
		Claim:  issueClaimFromAPI(lease),
		HubNow: claimHubNow(lease),
	})
}

func claimStatusFromAPI(resp api.ClaimStatusBody) db.ClaimStatus {
	lease := resp.Lease
	if lease == nil {
		lease = resp.Claim
	}
	return db.ClaimStatus{
		Held:   resp.Held,
		Holder: claimPrincipalFromAPI(resp.Holder),
		Claim:  issueClaimFromAPI(lease),
		HubNow: resp.HubNow,
	}
}

func claimPrincipalFromAPI(p api.ClaimPrincipalOut) db.ClaimPrincipal {
	return db.ClaimPrincipal{
		HolderInstanceUID: p.HolderInstanceUID,
		Holder:            p.Holder,
		ClientKind:        p.ClientKind,
	}
}

func issueClaimFromAPI(claim *api.IssueClaimOut) *db.IssueClaim {
	if claim == nil {
		return nil
	}
	out := &db.IssueClaim{
		ClaimUID:          claim.ClaimUID,
		ProjectID:         claim.ProjectID,
		IssueUID:          claim.IssueUID,
		Holder:            claim.Holder,
		HolderInstanceUID: claim.HolderInstanceUID,
		ClientKind:        claim.ClientKind,
		Purpose:           claim.Purpose,
		ClaimKind:         claim.ClaimKind,
		AcquiredAt:        claim.AcquiredAt,
		ExpiresAt:         claim.ExpiresAt,
		ReleasedAt:        claim.ReleasedAt,
		ReleaseReason:     claim.ReleaseReason,
		Revision:          claim.Revision,
		UpdatedAt:         claim.UpdatedAt,
	}
	return out
}

func claimHubNow(claim *api.IssueClaimOut) time.Time {
	if claim != nil && !claim.UpdatedAt.IsZero() {
		return claim.UpdatedAt
	}
	return time.Now().UTC()
}

func claimForwardError(err error) error {
	var statusErr *claimHubStatusError
	if errors.As(err, &statusErr) {
		return api.NewError(statusErr.StatusCode, "hub_claim_failed", statusErr.Error(), "", nil)
	}
	return api.NewError(http.StatusServiceUnavailable, "federation_offline", err.Error(), "", nil)
}

func isTransportClaimError(err error) bool {
	var statusErr *claimHubStatusError
	return !errors.As(err, &statusErr)
}

type claimHubClient struct {
	baseURL      string
	client       *http.Client
	transportErr error
}

type claimHubStatusError struct {
	Path       string
	StatusCode int
	Body       string
}

func (e *claimHubStatusError) Error() string {
	return fmt.Sprintf("hub %s returned %d: %s", e.Path, e.StatusCode, e.Body)
}

var errClaimHubTransportUnavailable = errors.New("claim hub transport unavailable")

func newClaimHubClient(ctx context.Context, baseURL, token string) (*claimHubClient, error) {
	httpClient, err := newClaimHubHTTPClient(ctx, baseURL)
	if err != nil {
		if errors.Is(err, errClaimHubTransportUnavailable) {
			return &claimHubClient{
				baseURL:      strings.TrimRight(baseURL, "/"),
				client:       &http.Client{Timeout: 10 * time.Second},
				transportErr: err,
			}, nil
		}
		return nil, err
	}
	if err := config.ConfigureBearerClient(httpClient, baseURL, token); err != nil {
		return nil, err
	}
	return &claimHubClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  httpClient,
	}, nil
}

func newClaimHubHTTPClient(ctx context.Context, baseURL string) (*http.Client, error) {
	if !strings.HasPrefix(baseURL, "http://kata.invalid") {
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	ns, err := NewNamespace()
	if err != nil {
		return nil, err
	}
	recs, err := ListRuntimeFiles(ns.DataDir)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		if !ProcessAlive(rec.PID) || !strings.HasPrefix(rec.Address, "unix://") {
			continue
		}
		path := strings.TrimPrefix(rec.Address, "unix://")
		probe := &http.Client{Transport: claimUnixTransport(path), Timeout: time.Second}
		if !claimHubPing(ctx, probe) {
			continue
		}
		return &http.Client{Transport: claimUnixTransport(path), Timeout: 10 * time.Second}, nil
	}
	return nil, fmt.Errorf("%w: no unix-socket daemon found", errClaimHubTransportUnavailable)
}

func claimHubPing(ctx context.Context, client *http.Client) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kata.invalid/api/v1/ping", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req) //nolint:gosec // Unix runtime file target is locally discovered and probed.
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

func claimUnixTransport(path string) *http.Transport {
	return &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

func (c *claimHubClient) AcquireClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req api.ClaimActionBody,
) (api.ClaimActionResponseBody, error) {
	return c.claimAction(ctx, hubProjectID, ref, "acquire", req)
}

func (c *claimHubClient) RenewClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req api.ClaimActionBody,
) (api.ClaimActionResponseBody, error) {
	return c.claimAction(ctx, hubProjectID, ref, "renew", req)
}

func (c *claimHubClient) ReleaseClaim(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	req api.ClaimActionBody,
) (api.ClaimActionResponseBody, error) {
	return c.claimAction(ctx, hubProjectID, ref, "release", req)
}

func (c *claimHubClient) ClaimStatus(ctx context.Context, hubProjectID int64, ref string) (api.ClaimStatusBody, error) {
	var body api.ClaimStatusBody
	err := c.getJSON(ctx, claimHubPath(hubProjectID, ref, "lease"), &body)
	return body, err
}

func (c *claimHubClient) claimAction(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	action string,
	req api.ClaimActionBody,
) (api.ClaimActionResponseBody, error) {
	var body api.ClaimActionResponseBody
	err := c.postJSON(ctx, claimHubPath(hubProjectID, ref, "lease/actions/"+action), req, &body)
	return body, err
}

func (c *claimHubClient) getJSON(ctx context.Context, path string, out any) error {
	if c.transportErr != nil {
		return c.transportErr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil) //nolint:gosec // hub URL comes from local federation config.
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req) //nolint:gosec // request target is an explicit configured hub.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &claimHubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *claimHubClient) postJSON(ctx context.Context, path string, in, out any) error {
	if c.transportErr != nil {
		return c.transportErr
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body)) //nolint:gosec // hub URL comes from local federation config.
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req) //nolint:gosec // request target is an explicit configured hub.
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &claimHubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func claimHubPath(hubProjectID int64, ref, suffix string) string {
	return fmt.Sprintf("/api/v1/projects/%d/issues/%s/%s", hubProjectID, url.PathEscape(ref), suffix)
}

func requireHubClaimBinding(ctx context.Context, store db.Storage, projectID int64) error {
	if _, err := activeProjectByID(ctx, store, projectID); err != nil {
		return err
	}
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return api.NewError(http.StatusNotFound, "federation_not_found", "project is not a federation hub", "", nil)
	}
	if err != nil {
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleHub {
		return api.NewError(http.StatusConflict, "federated_read_only", "claim actions must be resolved by the hub project", "", nil)
	}
	return nil
}

func claimKindOrDefault(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "hard"
	}
	return kind
}

func ttlDuration(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func claimAPIError(err error) error {
	switch {
	case errors.Is(err, db.ErrClaimDenied):
		return api.NewError(http.StatusConflict, "claim_denied", err.Error(), "", nil)
	case errors.Is(err, db.ErrClaimRequired):
		return api.NewError(http.StatusConflict, "claim_required", err.Error(), "", nil)
	case errors.Is(err, db.ErrClaimNotHeld):
		return api.NewError(http.StatusConflict, "claim_not_held", err.Error(), "", nil)
	case errors.Is(err, db.ErrClaimExpired):
		return api.NewError(http.StatusConflict, "claim_expired", err.Error(), "", nil)
	case errors.Is(err, db.ErrClaimValidation):
		return api.NewError(http.StatusBadRequest, "validation", err.Error(), "", nil)
	case errors.Is(err, db.ErrNotFound):
		return api.NewError(http.StatusNotFound, "issue_not_found", "issue not found", "", nil)
	default:
		return api.NewError(http.StatusInternalServerError, "internal", err.Error(), "", nil)
	}
}

func claimResultBody(result db.LeaseResult) api.ClaimActionResponseBody {
	lease := issueClaimOut(result.Claim)
	return api.ClaimActionResponseBody{
		Granted: result.Granted,
		Holder:  claimPrincipalOut(result.Holder),
		Lease:   lease,
		Claim:   lease,
		Event:   result.Event,
	}
}

func claimStatusBody(status db.ClaimStatus) api.ClaimStatusBody {
	lease := issueClaimOut(status.Claim)
	return api.ClaimStatusBody{
		Held:   status.Held,
		Holder: claimPrincipalOut(status.Holder),
		Lease:  lease,
		Claim:  lease,
		HubNow: status.HubNow,
	}
}

const showClaimStatusRetryAfter = time.Minute

func showIssueClaimRelevant(ctx context.Context, store db.Storage, projectID int64) (bool, error) {
	binding, err := store.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return binding.Enabled && (binding.Role == db.FederationRoleHub || binding.Role == db.FederationRoleSpoke), nil
}

func refreshShowClaimStatus(ctx context.Context, cfg ServerConfig, issue db.Issue) (*time.Time, error) {
	binding, err := cfg.DB.FederationBindingByProject(ctx, issue.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke {
		if binding.Enabled && binding.Role == db.FederationRoleHub {
			status, err := cfg.DB.ClaimStatus(ctx, issue.ProjectID, issue.UID, time.Now().UTC())
			if err != nil {
				return nil, claimAPIError(err)
			}
			emitClaimEvents(cfg, status.Events)
			hubNow := status.HubNow
			if hubNow.IsZero() {
				return nil, nil
			}
			return &hubNow, nil
		}
		return nil, nil
	}
	now := time.Now().UTC()
	if skip, err := skipRecentShowClaimStatusError(ctx, cfg.DB, issue, now); err != nil {
		return nil, api.NewError(500, "internal", err.Error(), "", nil)
	} else if skip {
		return nil, nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg.DB, binding)
	if err != nil {
		if isOfflineClaimRefreshError(err) {
			return nil, nil
		}
		return nil, err
	}
	resp, err := remote.ClaimStatus(ctx, cred.HubProjectID, issue.ShortID)
	if err != nil {
		var statusErr *claimHubStatusError
		if errors.As(err, &statusErr) {
			return nil, markShowClaimStatusError(ctx, cfg.DB, issue, statusErr, now)
		}
		return nil, markShowClaimStatusRefreshFailure(ctx, cfg.DB, issue, 0,
			fmt.Sprintf("status refresh transport: %s", err.Error()), now)
	}
	if err := cfg.DB.ApplyClaimStatus(ctx, binding.ProjectID, issue.UID, claimStatusFromAPI(resp)); err != nil {
		return nil, claimAPIError(err)
	}
	if err := cfg.DB.ClearClaimStatusRefreshError(ctx, issue.ProjectID, issue.UID); err != nil {
		return nil, claimAPIError(err)
	}
	if resp.HubNow.IsZero() {
		return nil, nil
	}
	hubNow := resp.HubNow
	return &hubNow, nil
}

func skipRecentShowClaimStatusError(ctx context.Context, store db.Storage, issue db.Issue, now time.Time) (bool, error) {
	statusErr, err := store.ClaimStatusRefreshError(ctx, issue.ProjectID, issue.UID)
	if err == nil && now.Sub(statusErr.LastAttemptAt.UTC()) < showClaimStatusRetryAfter {
		return true, nil
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return false, err
	}
	pending, err := store.ListPendingClaimRequestsForIssue(ctx, issue.ProjectID, issue.UID, 0)
	if err != nil {
		return false, err
	}
	for _, req := range pending {
		if req.LastAttemptAt == nil || req.LastError == nil {
			continue
		}
		if strings.HasPrefix(*req.LastError, "status refresh ") &&
			now.Sub(req.LastAttemptAt.UTC()) < showClaimStatusRetryAfter {
			return true, nil
		}
	}
	return false, nil
}

func markShowClaimStatusError(
	ctx context.Context,
	store db.Storage,
	issue db.Issue,
	statusErr *claimHubStatusError,
	now time.Time,
) error {
	msg := fmt.Sprintf("status refresh %s: %s", http.StatusText(statusErr.StatusCode), statusErr.Error())
	return markShowClaimStatusRefreshFailure(ctx, store, issue, statusErr.StatusCode, msg, now)
}

func markShowClaimStatusRefreshFailure(
	ctx context.Context,
	store db.Storage,
	issue db.Issue,
	statusCode int,
	msg string,
	now time.Time,
) error {
	if err := store.MarkClaimStatusRefreshError(ctx, issue.ProjectID, issue.UID, statusCode, msg, now); err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	pending, err := store.ListPendingClaimRequestsForIssue(ctx, issue.ProjectID, issue.UID, 0)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	for _, req := range pending {
		if err := store.MarkPendingClaimAttempt(ctx, req.RequestUID, msg, now); err != nil {
			return api.NewError(500, "internal", err.Error(), "", nil)
		}
	}
	return nil
}

func hydrateClaimOutForIssue(ctx context.Context, cfg ServerConfig, issue db.Issue, out *api.ShowIssueResponse) error {
	now := time.Now().UTC()
	status, err := cfg.DB.ClaimStatusReadOnly(ctx, issue.ProjectID, issue.UID, now)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	pending, err := cfg.DB.ListPendingClaimRequestsForIssue(ctx, issue.ProjectID, issue.UID, 0)
	if err != nil {
		return api.NewError(500, "internal", err.Error(), "", nil)
	}
	out.Body.Claim = issueClaimOut(status.Claim)
	out.Body.Lease = out.Body.Claim
	if len(pending) > 0 {
		out.Body.PendingClaims = pendingClaimOuts(pending)
		out.Body.PendingLeases = out.Body.PendingClaims
	}
	if out.Body.Lease != nil || len(pending) > 0 {
		hubNow := status.HubNow
		if hubNow.IsZero() {
			hubNow = now
		}
		out.Body.ClaimHubNow = &hubNow
		out.Body.LeaseHubNow = &hubNow
	}
	return nil
}

func pendingClaimOuts(pending []db.PendingClaimRequest) []api.PendingClaimOut {
	out := make([]api.PendingClaimOut, 0, len(pending))
	for _, req := range pending {
		out = append(out, api.PendingClaimOut{
			RequestUID:        req.RequestUID,
			Holder:            req.Holder,
			HolderInstanceUID: req.HolderInstanceUID,
			ClientKind:        req.ClientKind,
			ClaimKind:         req.ClaimKind,
			TTLSeconds:        req.TTLSeconds,
			Purpose:           req.Purpose,
			RequestedAt:       req.RequestedAt,
			LastAttemptAt:     req.LastAttemptAt,
			LastError:         req.LastError,
		})
	}
	return out
}

func claimPrincipalOut(p db.ClaimPrincipal) api.ClaimPrincipalOut {
	return api.ClaimPrincipalOut{
		HolderInstanceUID: p.HolderInstanceUID,
		Holder:            p.Holder,
		ClientKind:        p.ClientKind,
	}
}

func issueClaimOut(claim *db.IssueClaim) *api.IssueClaimOut {
	if claim == nil {
		return nil
	}
	return &api.IssueClaimOut{
		ClaimUID:          claim.ClaimUID,
		ProjectID:         claim.ProjectID,
		IssueUID:          claim.IssueUID,
		Holder:            claim.Holder,
		HolderInstanceUID: claim.HolderInstanceUID,
		ClientKind:        claim.ClientKind,
		Purpose:           claim.Purpose,
		ClaimKind:         claim.ClaimKind,
		AcquiredAt:        claim.AcquiredAt,
		ExpiresAt:         claim.ExpiresAt,
		ReleasedAt:        claim.ReleasedAt,
		ReleaseReason:     claim.ReleaseReason,
		Revision:          claim.Revision,
		UpdatedAt:         claim.UpdatedAt,
	}
}

func emitClaimEvent(cfg ServerConfig, projectID int64, event *db.Event) {
	if event == nil {
		return
	}
	cfg.Broadcaster.Broadcast(StreamMsg{Kind: "event", Event: event, ProjectID: projectID})
	cfg.Hooks.Enqueue(*event)
}

func emitClaimEvents(cfg ServerConfig, events []db.Event) {
	for i := range events {
		emitClaimEvent(cfg, events[i].ProjectID, &events[i])
	}
}
