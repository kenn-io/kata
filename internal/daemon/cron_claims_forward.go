package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/uid"
)

func cronClaimPending() error {
	return api.NewError(503, "cron_claim_pending", db.ErrCronClaimPending.Error(), "keep pending; do not launch", nil)
}

func cronClaimBinding(ctx context.Context, cfg ServerConfig, p int64) (db.FederationBinding, bool, error) {
	b, e := cfg.DB.FederationBindingByProject(ctx, p)
	if errors.Is(e, db.ErrNotFound) {
		return b, false, nil
	}
	if e != nil {
		return b, false, internalAPIError(e)
	}
	if !b.Enabled {
		return b, false, cronClaimPending()
	}
	switch b.Role {
	case db.FederationRoleHub:
		return b, false, nil
	case db.FederationRoleSpoke:
		return b, true, nil
	default:
		return b, false, cronClaimPending()
	}
}

// The existing claim transport pins the enrollment bearer to its configured origin,
// rejects redirects, and supports federation signing and Unix sockets.
func (c *claimHubClient) cronRequest(ctx context.Context, method, path string, body any, hops int) (int, []byte, error) {
	if c.transportErr != nil {
		return 0, nil, c.transportErr
	}
	if hops >= 16 {
		return 0, nil, db.ErrCronClaimPending
	}
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Kata-Cron-Hops", strconv.Itoa(hops+1))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req) //nolint:gosec // The operator-configured federation transport pins the enrollment bearer to this origin.
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil {
		return 0, nil, err
	}
	if len(raw) > 64*1024 {
		return 0, nil, db.ErrCronClaimPending
	}
	return resp.StatusCode, raw, nil
}

type cronClaimCapabilities struct {
	ProjectUID   string   `json:"project_uid"`
	Capabilities []string `json:"capabilities"`
	Authority    string   `json:"claim_authority_instance_uid"`
}

func forwardCronCapabilities(ctx context.Context, cfg ServerConfig, b db.FederationBinding, hops int) (*claimHubClient, int64, cronClaimCapabilities, error) {
	remote, cred, err := claimForwardClient(ctx, cfg, b)
	if err != nil {
		return nil, 0, cronClaimCapabilities{}, cronClaimPending()
	}
	code, raw, err := remote.cronRequest(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/capabilities", cred.HubProjectID), nil, hops)
	var caps cronClaimCapabilities
	if err != nil || code != 200 || json.Unmarshal(raw, &caps) != nil || caps.ProjectUID != b.HubProjectUID || !uid.Valid(caps.Authority) || !slices.Contains(caps.Capabilities, cronOccurrenceClaimsCapability) {
		return nil, 0, caps, cronClaimPending()
	}
	if err := cfg.DB.CheckCronClaimUpstream(ctx, b.ProjectID, caps.Authority); err != nil {
		return nil, 0, caps, cronClaimPending()
	}
	return remote, cred.HubProjectID, caps, nil
}

func effectiveCronClaimCapability(ctx context.Context, cfg ServerConfig, p int64, hops int) (string, error) {
	if hops >= 16 {
		return "", db.ErrCronClaimPending
	}
	finish, err := beginClaimFederationTransport(ctx, cfg, p)
	if err != nil {
		return "", err
	}
	defer finish()
	b, forward, err := cronClaimBinding(ctx, cfg, p)
	if err != nil {
		return "", err
	}
	if !forward {
		return cfg.DB.CronClaimAuthority(ctx, p)
	}
	_, _, caps, err := forwardCronCapabilities(ctx, cfg, b, hops)
	return caps.Authority, err
}

func cronClaimOrigin(cfg ServerConfig, principal claimPrincipal, provided *api.CronClaimOrigin) (api.CronClaimOrigin, error) {
	origin := api.CronClaimOrigin{HolderInstanceUID: principal.HolderInstanceUID, Actor: principal.Holder, Path: []string{principal.HolderInstanceUID}}
	if provided == nil {
		return origin, nil
	}
	fed := principal.Federation
	if !principal.Enrollment || fed == nil || fed.RelayProtocolVersion != db.RelayProtocolVersion || fed.RelayBindingUID == "" || !fed.RelayServeDownstream {
		return origin, federationCredentialDenied()
	}
	origin = *provided
	if !uid.Valid(origin.HolderInstanceUID) || len(origin.Path) == 0 || len(origin.Path) > 16 || origin.Path[0] != origin.HolderInstanceUID || origin.Path[len(origin.Path)-1] != principal.HolderInstanceUID {
		return origin, federationCredentialDenied()
	}
	seen := map[string]bool{}
	for _, node := range origin.Path {
		if !uid.Valid(node) || seen[node] || node == cfg.DB.InstanceUID() {
			return origin, federationCredentialDenied()
		}
		seen[node] = true
	}
	return origin, nil
}

func forwardCronClaim(ctx context.Context, cfg ServerConfig, b db.FederationBinding, principal claimPrincipal, origin api.CronClaimOrigin, job string, body api.CronClaimBody, hops int) (*api.CronClaimResponse, error) {
	if b.RelayConfig == nil && principal.Enrollment {
		return nil, cronClaimPending()
	}
	remote, project, caps, err := forwardCronCapabilities(ctx, cfg, b, hops)
	if err != nil {
		return nil, err
	}
	if body.ExpectedAuthorityInstanceUID != "" && body.ExpectedAuthorityInstanceUID != caps.Authority {
		return nil, cronClaimPending()
	}
	// This transaction runs incoming token/enrollment/host fences before the relay
	// may use its upstream credential. It ends before network I/O.
	if err := cfg.DB.AdmitCronClaim(ctx, b.ProjectID, caps.Authority); err != nil {
		return nil, cronClaimAPIError(ctx, cfg, b.ProjectID, db.CronClaimResult{}, err)
	}
	expectedHolder := cfg.DB.InstanceUID()
	expectedActor := b.Actor
	if b.RelayConfig != nil {
		path := slices.Clone(origin.Path)
		if path[len(path)-1] != cfg.DB.InstanceUID() {
			if slices.Contains(path, cfg.DB.InstanceUID()) {
				return nil, cronClaimPending()
			}
			path = append(path, cfg.DB.InstanceUID())
		}
		if len(path) > 16 {
			return nil, cronClaimPending()
		}
		origin.Path = path
		body.Origin = &origin
		expectedHolder = origin.HolderInstanceUID
		expectedActor = origin.Actor
	} else {
		body.Origin = nil
	}
	body.Actor = b.Actor
	body.ExpectedAuthorityInstanceUID = caps.Authority
	code, raw, err := remote.cronRequest(ctx, http.MethodPost, fmt.Sprintf("/api/v1/projects/%d/cron/jobs/%s/claims", project, url.PathEscape(job)), body, hops)
	if err != nil {
		return nil, cronClaimPending()
	}
	if code == 200 || code == 201 {
		var out api.CronClaimResponseBody
		if json.Unmarshal(raw, &out) != nil || !out.Claimed || !validForwardedCronClaim(out.Claim, project, caps.Authority) || out.Claim.RunUID != strings.ToUpper(body.RunUID) || out.Claim.JobUID != strings.ToUpper(job) || out.Claim.OccurrenceKey != body.OccurrenceKey || out.Claim.HolderInstanceUID != expectedHolder || !cronClaimRequestEvidenceMatches(out.Claim, body) || (expectedActor != "" && out.Claim.Actor != expectedActor) || (code == 201 && out.Replayed) || (code == 200 && !out.Replayed) {
			return nil, cronClaimPending()
		}
		out.Claim.ProjectID = b.ProjectID
		return &api.CronClaimResponse{Status: code, Body: out}, nil
	}
	var envelope api.ErrorEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, cronClaimPending()
	}
	switch {
	case code == 409 && envelope.Error.Code == "cron_occurrence_claimed":
		encoded, err := json.Marshal(envelope.Error.Data["claim"])
		if err != nil {
			return nil, cronClaimPending()
		}
		var claim db.CronOccurrenceClaim
		if json.Unmarshal(encoded, &claim) != nil || !validForwardedCronClaim(claim, project, caps.Authority) || claim.JobUID != strings.ToUpper(job) || claim.OccurrenceKey != body.OccurrenceKey {
			return nil, cronClaimPending()
		}
		claim.ProjectID = b.ProjectID
		return nil, api.NewError(409, "cron_occurrence_claimed", db.ErrCronOccurrenceClaimed.Error(), "skip this occurrence", map[string]any{"claim": claim})
	case code == 409 && envelope.Error.Code == "cron_claim_identity_conflict":
		return nil, api.NewError(409, envelope.Error.Code, db.ErrCronClaimIdentity.Error(), "reuse the original immutable request", nil)
	case code == 400 && envelope.Error.Code == "validation":
		return nil, api.NewError(400, "validation", envelope.Error.Message, "", nil)
	default:
		return nil, cronClaimPending()
	}
}

func validForwardedCronClaim(c db.CronOccurrenceClaim, p int64, authority string) bool {
	return c.ProjectID == p && c.AuthorityInstanceUID == authority && db.ValidateCronClaim(c) == nil
}

func forwardCronClaimLookup(ctx context.Context, cfg ServerConfig, b db.FederationBinding, id string, hops int) (*api.CronClaimResponse, error) {
	remote, p, caps, err := forwardCronCapabilities(ctx, cfg, b, hops)
	if err != nil {
		return nil, err
	}
	code, raw, err := remote.cronRequest(ctx, http.MethodGet, fmt.Sprintf("/api/v1/projects/%d/cron/claims/%s", p, url.PathEscape(id)), nil, hops)
	if err != nil {
		return nil, cronClaimPending()
	}
	if code == 404 {
		return nil, api.NewError(404, "not_found", "cron claim not found", "", nil)
	}
	var out api.CronClaimResponseBody
	if code != 200 || json.Unmarshal(raw, &out) != nil || !out.Claimed || !validForwardedCronClaim(out.Claim, p, caps.Authority) || out.Claim.RunUID != strings.ToUpper(id) {
		return nil, cronClaimPending()
	}
	out.Claim.ProjectID = b.ProjectID
	return &api.CronClaimResponse{Status: 200, Body: out}, nil
}

// A 2xx response must confirm the same immutable request, not merely its UID.
func cronClaimRequestEvidenceMatches(c db.CronOccurrenceClaim, b api.CronClaimBody) bool {
	return c.DefinitionEventUID == strings.ToUpper(b.DefinitionEventUID) && reflect.DeepEqual(c.Teammate, b.Teammate) && reflect.DeepEqual(c.ExecutorLabel, b.ExecutorLabel) && reflect.DeepEqual(c.WorkflowUID, b.WorkflowUID) && reflect.DeepEqual(c.WorkflowDefinitionEventUID, b.WorkflowDefinitionEventUID)
}
