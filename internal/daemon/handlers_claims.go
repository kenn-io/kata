package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"go.kenn.io/kata/pkg/client/generated"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
)

// projectScopedClaimActionEvent rewrites the lease action response's event
// through the typed issue-scoped projection. Unscoped callers are untouched.
func projectScopedClaimActionEvent(
	ctx context.Context, store db.Storage, body *api.ClaimActionResponseBody,
) error {
	if issueScopeFromContext(ctx) == nil || body == nil || body.Event == nil {
		return nil
	}
	projected, err := scopedMutationEvent(ctx, store, body.Event)
	if err != nil {
		return err
	}
	body.Event = projected
	return nil
}

func registerClaimHandlers(humaAPI huma.API, cfg ServerConfig) {
	huma.Register(humaAPI, huma.Operation{
		OperationID: "acquireIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/acquire",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		ctx, principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body,
			federationTransportOperation("acquireIssueLease"), true)
		if err != nil {
			return nil, err
		}
		if _, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo); err != nil {
			return nil, err
		}
		body, err := handleClaimAcquire(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		if err := projectScopedClaimActionEvent(ctx, cfg.DB, &body); err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "renewIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/renew",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		ctx, principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body,
			federationTransportOperation("renewIssueLease"), true)
		if err != nil {
			return nil, err
		}
		if _, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo); err != nil {
			return nil, err
		}
		body, err := handleClaimRenew(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		if err := projectScopedClaimActionEvent(ctx, cfg.DB, &body); err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "releaseIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/release",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		ctx, principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body,
			federationTransportOperation("releaseIssueLease"), true)
		if err != nil {
			return nil, err
		}
		if _, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo); err != nil {
			return nil, err
		}
		body, err := handleClaimRelease(ctx, cfg, in.ProjectID, in.Ref, in.Body, principal.ClaimPrincipal)
		if err != nil {
			return nil, err
		}
		if err := projectScopedClaimActionEvent(ctx, cfg.DB, &body); err != nil {
			return nil, err
		}
		return &api.ClaimActionResponse{Body: body}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "forceReleaseIssueLease",
		Method:      http.MethodPost,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease/actions/force_release",
	}, func(ctx context.Context, in *api.ClaimActionRequest) (*api.ClaimActionResponse, error) {
		ctx, principal, err := resolveClaimPrincipal(ctx, cfg, in.ProjectID, in.Authorization, in.Body,
			federationTransportOperation("forceReleaseIssueLease"), false)
		if err != nil {
			return nil, err
		}
		if issueScopeFromContext(ctx) != nil {
			return nil, api.NewError(http.StatusForbidden, "scoped_operation_forbidden",
				"operation is not available to an issue-scoped credential", "", nil)
		}
		if err := requireHubClaimBinding(ctx, cfg.DB, in.ProjectID); err != nil {
			return nil, err
		}
		actor := actorFor(ctx, strings.TrimSpace(in.Body.Actor))
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
				cfg.Publish().EventsByProject(result.Events)
			}
			return nil, claimAPIError(err)
		}
		cfg.Publish().EventsByProject(result.Events)
		if result.Event != nil {
			cfg.Publish().Event(in.ProjectID, *result.Event)
		}
		return &api.ClaimActionResponse{Body: claimResultBody(result)}, nil
	})

	huma.Register(humaAPI, huma.Operation{
		OperationID: "getIssueLeaseStatus",
		Method:      http.MethodGet,
		Path:        "/api/v1/projects/{project_id}/issues/{ref}/lease",
	}, func(ctx context.Context, in *api.ClaimStatusRequest) (*api.ClaimStatusResponse, error) {
		var err error
		ctx, err = authorizeClaimStatusRead(ctx, cfg, in.ProjectID, in.Authorization)
		if err != nil {
			return nil, err
		}
		if _, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo); err != nil {
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
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	defer finishTransport()

	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	principal = boundSpokeClaimPrincipal(binding, principal)
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
		cfg.Publish().EventsByProject(result.Events)
		if result.Event != nil {
			cfg.Publish().Event(projectID, *result.Event)
		}
		if err := federationFailpoint("after_claim_grant_commit_before_response"); err != nil {
			return api.ClaimActionResponseBody{}, api.NewError(http.StatusInternalServerError, "federation_failpoint", err.Error(), "", nil)
		}
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
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
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	defer finishTransport()

	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	principal = boundSpokeClaimPrincipal(binding, principal)
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
				cfg.Publish().EventsByProject(result.Events)
			}
			return api.ClaimActionResponseBody{}, claimAPIError(err)
		}
		cfg.Publish().EventsByProject(result.Events)
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
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
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	defer finishTransport()

	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	principal = boundSpokeClaimPrincipal(binding, principal)
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
				cfg.Publish().EventsByProject(result.Events)
			}
			return api.ClaimActionResponseBody{}, claimAPIError(err)
		}
		cfg.Publish().EventsByProject(result.Events)
		if result.Event != nil {
			cfg.Publish().Event(projectID, *result.Event)
		}
		return claimResultBody(result), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
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
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, projectID)
	if err != nil {
		return api.ClaimStatusBody{}, err
	}
	defer finishTransport()

	binding, err := claimBinding(ctx, cfg.DB, projectID)
	if err != nil {
		return api.ClaimStatusBody{}, err
	}
	if binding.Role == db.FederationRoleHub {
		status, err := cfg.DB.ClaimStatus(ctx, projectID, ref, time.Now().UTC())
		if err != nil {
			return api.ClaimStatusBody{}, claimAPIError(err)
		}
		cfg.Publish().EventsByProject(status.Events)
		return claimStatusBody(status), nil
	}
	remote, cred, err := claimForwardClient(ctx, cfg, binding)
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
		return db.FederationBinding{}, internalAPIError(err)
	}
	if !binding.Enabled {
		return db.FederationBinding{}, api.NewError(http.StatusConflict, "federated_read_only", "federation binding is disabled", "", nil)
	}
	if binding.Role != db.FederationRoleHub && binding.Role != db.FederationRoleSpoke {
		return db.FederationBinding{}, api.NewError(http.StatusConflict, "federated_read_only", "unknown federation binding role", "", nil)
	}
	return binding, nil
}

func beginClaimFederationTransport(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
) (func(), error) {
	finish, err := federationcoord.BeginSync(
		ctx,
		federationcoord.Key(cfg.DB.InstanceUID(), projectID),
		cfg.DB,
		projectID,
	)
	if err != nil {
		return nil, internalAPIError(fmt.Errorf("coordinate federation claim transport: %w", err))
	}
	return finish, nil
}

func boundSpokeClaimPrincipal(binding db.FederationBinding, principal db.ClaimPrincipal) db.ClaimPrincipal {
	if binding.Role != db.FederationRoleSpoke {
		return principal
	}
	actor := strings.TrimSpace(binding.Actor)
	if actor == "" {
		return principal
	}
	// Existing clients keep their established actor/client-kind identity.
	// Mounted callers carry an opaque subject-bound identity through the shared
	// spoke credential so one subject cannot control another subject's lease.
	if principal.AuthenticatedHost {
		ownerDigest := sha256.Sum256([]byte("kata:spoke-host-claim-owner:v1\x00" + principal.Holder))
		principal.ClientKind = "spoke-host:v1:" + base64.RawURLEncoding.EncodeToString(ownerDigest[:])
	}
	principal.Holder = actor
	return principal
}

func claimForwardClient(
	ctx context.Context,
	cfg ServerConfig,
	binding db.FederationBinding,
) (*claimHubClient, config.FederationCredential, error) {
	project, err := cfg.DB.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return nil, config.FederationCredential{}, claimAPIError(err)
	}
	cred, _, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil {
		return nil, config.FederationCredential{}, internalAPIError(err)
	}
	if strings.TrimSpace(cred.Token) == "" {
		return nil, config.FederationCredential{}, api.NewError(http.StatusServiceUnavailable, "federation_offline", "federation claim credentials are unavailable", "", nil)
	}
	cred = config.FederationTransportCredential(
		binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred,
	)
	client, err := newClaimHubClient(ctx, cred.HubURL, cred.Token, cred.AllowInsecure)
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
	if statusErr, ok := errors.AsType[*claimHubStatusError](err); ok {
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

func newClaimHubClient(ctx context.Context, baseURL, token string, allowInsecure bool) (*claimHubClient, error) {
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
	if err := configureClaimHubBearerClient(httpClient, baseURL, token, allowInsecure); err != nil {
		return nil, err
	}
	return &claimHubClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  httpClient,
	}, nil
}

// configureClaimHubBearerClient pins the hub's own token to the hub origin.
// This is a hub client, never the local daemon: the local daemon's global
// token must never reach it.
func configureClaimHubBearerClient(c *http.Client, baseURL, token string, allowInsecure bool) error {
	policy := config.BearerPolicy{AllowInsecurePlaintext: allowInsecure}
	if !allowInsecure {
		policy.TrustPrivateNetwork = config.ResolvedBearerTrustPrivateNetwork()
	}
	return policy.ConfigureClient(c, baseURL, token)
}

func newClaimHubHTTPClient(ctx context.Context, baseURL string) (*http.Client, error) {
	if !strings.HasPrefix(baseURL, "http://kata.invalid") {
		return &http.Client{Timeout: 10 * time.Second}, nil
	}
	ns, err := NewNamespace()
	if err != nil {
		return nil, err
	}
	recs, err := (kitdaemon.RuntimeStore{Dir: ns.DataDir}).List()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errClaimHubTransportUnavailable, err)
	}
	for _, rec := range recs {
		if !RuntimeProcessAlive(rec) {
			continue
		}
		ep := rec.Endpoint()
		if !ep.IsUnix() {
			continue
		}
		path := ep.Address
		probe := &http.Client{Transport: claimUnixTransport(path), Timeout: time.Second}
		if !claimHubPing(ctx, probe) {
			continue
		}
		return &http.Client{Transport: claimUnixTransport(path), Timeout: 10 * time.Second}, nil
	}
	return nil, fmt.Errorf("%w: no unix-socket daemon found", errClaimHubTransportUnavailable)
}

func claimHubPing(ctx context.Context, client *http.Client) bool {
	apiClient, err := generated.NewDefaultClient("http://kata.invalid", runtime.WithHTTPClient(claimRequestDoer{client}))
	if err != nil {
		return false
	}
	resp, _ := apiClient.PingWithResponse(ctx)
	return resp != nil && resp.StatusCode == http.StatusOK
}

type claimRequestDoer struct{ client *http.Client }

func (d claimRequestDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return d.client.Do(req.WithContext(ctx)) //nolint:gosec // G704: this generated ping probes only the discovered local daemon socket.
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

func (c *claimHubClient) ClaimIssue(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	body api.ClaimRequestBody,
) (api.ClaimResponseBody, error) {
	if c.transportErr != nil {
		return api.ClaimResponseBody{}, c.transportErr
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return api.ClaimResponseBody{}, err
	}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/claim",
		c.baseURL, hubProjectID, url.PathEscape(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return api.ClaimResponseBody{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req) //nolint:gosec // the binding's canonical hub origin configures this client and pins its bearer token.
	if err != nil {
		return api.ClaimResponseBody{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return api.ClaimResponseBody{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return api.ClaimResponseBody{}, &claimHubStatusError{
			Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw)),
		}
	}
	var result api.ClaimResponseBody
	if err := json.Unmarshal(raw, &result); err != nil {
		return api.ClaimResponseBody{}, err
	}
	return result, nil
}

// nonNilEvents upholds the claim response contract that events is a
// required array: it replaces a nil slice with the empty array so the
// response object keeps the invariant outside the daemon's JSON v2 writer,
// including with legacy encoders that render nil slices as null. Call at
// every ClaimResponseBody construction site.
func nonNilEvents(events []db.Event) []db.Event {
	if events == nil {
		return []db.Event{}
	}
	return events
}

func applyForwardedAssignmentClaim(
	ctx context.Context,
	cfg ServerConfig,
	projectID int64,
	issueUID string,
	forwarded api.ClaimResponseBody,
) (*api.ClaimResponse, error) {
	receivedEvents := make([]db.Event, 0, len(forwarded.ReplayEvents)+len(forwarded.Events))
	receivedEvents = append(receivedEvents, forwarded.ReplayEvents...)
	receivedEvents = append(receivedEvents, forwarded.Events...)
	allUIDs := make([]string, 0, len(forwarded.Events))
	for _, event := range forwarded.Events {
		allUIDs = append(allUIDs, event.UID)
	}
	insertedUIDs := make([]string, 0, len(receivedEvents))
	for _, event := range receivedEvents {
		inserted, err := cfg.DB.InsertRemoteEvent(ctx, projectID, remoteEventFromStoredEvent(event))
		if err != nil {
			return nil, err
		}
		if inserted {
			insertedUIDs = append(insertedUIDs, event.UID)
		}
	}
	if len(receivedEvents) > 0 {
		if err := cfg.DB.MaterializeFederatedProject(ctx, projectID); err != nil {
			return nil, err
		}
	}
	issue, err := cfg.DB.IssueByUID(ctx, issueUID, db.IncludeDeletedNo)
	if err != nil {
		return nil, err
	}
	events, err := cfg.DB.EventsByUIDs(ctx, projectID, allUIDs)
	if err != nil {
		return nil, err
	}
	insertedEvents, err := cfg.DB.EventsByUIDs(ctx, projectID, insertedUIDs)
	if err != nil {
		return nil, err
	}
	cfg.Publish().Events(projectID, insertedEvents)
	// Response events follow the scoped projection contract shared by every
	// mutation response; publishing above stays raw so mirror subscribers and
	// hooks keep the durable form. Remote event rows carry no local issue_id
	// (UIDs are canonical), so resolve the mirrored issues' local IDs first:
	// the scope membership check matches on them and fails closed on rows it
	// cannot anchor.
	if issueScopeFromContext(ctx) != nil {
		anchored := make([]db.Event, len(events))
		for i, event := range events {
			anchored[i] = event
			if event.IssueUID == nil {
				continue
			}
			mirrored, err := cfg.DB.IssueByUID(ctx, *event.IssueUID, db.IncludeDeletedYes)
			if err != nil {
				continue
			}
			anchored[i].IssueID = &mirrored.ID
		}
		events = anchored
	}
	responseEvents, _, err := scopedMutationEvents(ctx, cfg.DB, events)
	if err != nil {
		return nil, err
	}
	body := forwarded
	body.Issue = issue
	body.Events = nonNilEvents(responseEvents)
	body.ReplayEvents = nil
	body.Event = nil
	if len(responseEvents) > 0 {
		body.Event = &responseEvents[len(responseEvents)-1]
	}
	return &api.ClaimResponse{Body: body}, nil
}

func remoteEventFromStoredEvent(event db.Event) db.RemoteEvent {
	return db.RemoteEvent{
		EventUID:          event.UID,
		OriginInstanceUID: event.OriginInstanceUID,
		ProjectUID:        event.ProjectUID,
		ProjectName:       event.ProjectName,
		IssueUID:          event.IssueUID,
		RelatedIssueUID:   event.RelatedIssueUID,
		Type:              event.Type,
		Actor:             event.Actor,
		HLCPhysicalMS:     event.HLCPhysicalMS,
		HLCCounter:        event.HLCCounter,
		ContentHash:       event.ContentHash,
		Payload:           jsontext.Value(event.Payload),
		CreatedAt:         event.CreatedAt,
	}
}

func (c *claimHubClient) ClaimStatus(ctx context.Context, hubProjectID int64, ref string) (api.ClaimStatusBody, error) {
	if c.transportErr != nil {
		return api.ClaimStatusBody{}, c.transportErr
	}
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(claimHubDoer{c.client}))
	if err != nil {
		return api.ClaimStatusBody{}, err
	}
	response, callErr := apiClient.GetIssueLeaseStatusWithResponse(ctx, &generated.GetIssueLeaseStatusRequestOptions{PathParams: &generated.GetIssueLeaseStatusPath{ProjectID: hubProjectID, Ref: ref}})
	var body api.ClaimStatusBody
	if response == nil {
		return body, callErr
	}
	err = json.Unmarshal(response.Body, &body)
	if err == nil {
		normalizeForwardedClaimStatus(&body)
	}
	return body, err
}

func (c *claimHubClient) claimAction(
	ctx context.Context,
	hubProjectID int64,
	ref string,
	action string,
	req api.ClaimActionBody,
) (api.ClaimActionResponseBody, error) {
	if c.transportErr != nil {
		return api.ClaimActionResponseBody{}, c.transportErr
	}
	apiClient, err := generated.NewDefaultClient(c.baseURL, runtime.WithHTTPClient(claimHubDoer{c.client}))
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	var payload generated.ClaimActionBody
	if err := json.Unmarshal(data, &payload); err != nil {
		return api.ClaimActionResponseBody{}, err
	}
	var body api.ClaimActionResponseBody
	switch action {
	case "acquire":
		response, callErr := apiClient.AcquireIssueLeaseWithResponse(ctx, &generated.AcquireIssueLeaseRequestOptions{PathParams: &generated.AcquireIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	case "renew":
		response, callErr := apiClient.RenewIssueLeaseWithResponse(ctx, &generated.RenewIssueLeaseRequestOptions{PathParams: &generated.RenewIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	case "release":
		response, callErr := apiClient.ReleaseIssueLeaseWithResponse(ctx, &generated.ReleaseIssueLeaseRequestOptions{PathParams: &generated.ReleaseIssueLeasePath{ProjectID: hubProjectID, Ref: ref}, Body: &payload})
		if response == nil {
			return body, callErr
		}
		err = json.Unmarshal(response.Body, &body)
	default:
		return body, fmt.Errorf("unknown claim action %q", action)
	}
	if err == nil {
		normalizeForwardedClaimActionResponse(&body)
	}
	return body, err
}

func normalizeForwardedClaimActionResponse(body *api.ClaimActionResponseBody) {
	if body.Lease == nil {
		body.Lease = body.Claim
	}
	body.MirrorDeprecatedClaimFields()
}

func normalizeForwardedClaimStatus(body *api.ClaimStatusBody) {
	if body.Lease == nil {
		body.Lease = body.Claim
	}
	body.MirrorDeprecatedClaimFields()
}

// claimHubDoer keeps error bodies bounded while generated operations own requests.
type claimHubDoer struct{ client *http.Client }

func (d claimHubDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	resp, err := d.client.Do(req.WithContext(ctx)) //nolint:gosec // G704: generated lease routes use the operator-configured federation hub and its origin-pinned transport.
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &claimHubStatusError{Path: req.URL.Path, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	return resp, nil
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
		return internalAPIError(err)
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
	case errors.Is(err, db.ErrTransactionFinalizationFailed):
		return internalAPIError(err)
	case errors.Is(err, ErrHostAccessDenied):
		return federationCredentialDenied()
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
		return internalAPIError(err)
	}
}

func claimResultBody(result db.LeaseResult) api.ClaimActionResponseBody {
	body := api.ClaimActionResponseBody{
		Granted: result.Granted,
		Holder:  claimPrincipalOut(result.Holder),
		Lease:   issueClaimOut(result.Claim),
		Event:   result.Event,
	}
	body.MirrorDeprecatedClaimFields()
	return body
}

func claimStatusBody(status db.ClaimStatus) api.ClaimStatusBody {
	body := api.ClaimStatusBody{
		Held:   status.Held,
		Holder: claimPrincipalOut(status.Holder),
		Lease:  issueClaimOut(status.Claim),
		HubNow: status.HubNow,
	}
	body.MirrorDeprecatedClaimFields()
	return body
}

const showClaimStatusRetryAfter = time.Minute

// showClaimStatusRefreshTimeout is a var so tests can exercise the budget without waiting it out.
var showClaimStatusRefreshTimeout = 500 * time.Millisecond

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
	finishTransport, err := beginClaimFederationTransport(ctx, cfg, issue.ProjectID)
	if err != nil {
		return nil, err
	}
	defer finishTransport()

	binding, err := cfg.DB.FederationBindingByProject(ctx, issue.ProjectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, internalAPIError(err)
	}
	if !binding.Enabled || binding.Role != db.FederationRoleSpoke {
		if binding.Enabled && binding.Role == db.FederationRoleHub {
			status, err := cfg.DB.ClaimStatus(ctx, issue.ProjectID, issue.UID, time.Now().UTC())
			if err != nil {
				return nil, claimAPIError(err)
			}
			cfg.Publish().EventsByProject(status.Events)
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
		return nil, internalAPIError(err)
	} else if skip {
		return nil, nil
	}
	remoteCtx, cancelRemote := context.WithTimeout(ctx, showClaimStatusRefreshTimeout)
	remote, cred, err := claimForwardClient(remoteCtx, cfg, binding)
	if err != nil {
		budgetErr := remoteCtx.Err()
		cancelRemote()
		if budgetErr != nil {
			return nil, markShowClaimStatusRefreshFailure(ctx, cfg.DB, issue, 0,
				fmt.Sprintf("status refresh transport: %s", budgetErr.Error()), now)
		}
		if isOfflineClaimRefreshError(err) {
			return nil, nil
		}
		return nil, err
	}
	resp, err := remote.ClaimStatus(remoteCtx, cred.HubProjectID, issue.ShortID)
	cancelRemote()
	if err != nil {
		if statusErr, ok := errors.AsType[*claimHubStatusError](err); ok {
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
		return internalAPIError(err)
	}
	pending, err := store.ListPendingClaimRequestsForIssue(ctx, issue.ProjectID, issue.UID, 0)
	if err != nil {
		return internalAPIError(err)
	}
	for _, req := range pending {
		if err := store.MarkPendingClaimAttempt(ctx, req.RequestUID, msg, now); err != nil {
			return internalAPIError(err)
		}
	}
	return nil
}

func hydrateClaimOutForIssue(ctx context.Context, cfg ServerConfig, issue db.Issue, out *api.ShowIssueResponse) error {
	now := time.Now().UTC()
	status, err := cfg.DB.ClaimStatusReadOnly(ctx, issue.ProjectID, issue.UID, now)
	if err != nil {
		return internalAPIError(err)
	}
	pending, err := cfg.DB.ListPendingClaimRequestsForIssue(ctx, issue.ProjectID, issue.UID, 0)
	if err != nil {
		return internalAPIError(err)
	}
	out.Body.Lease = issueClaimOut(status.Claim)
	if len(pending) > 0 {
		out.Body.PendingLeases = pendingClaimOuts(pending)
	}
	if out.Body.Lease != nil || len(pending) > 0 {
		hubNow := status.HubNow
		if hubNow.IsZero() {
			hubNow = now
		}
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
