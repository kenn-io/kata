package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
	katauid "go.kenn.io/kata/internal/uid"
)

const federatedDocbankPath = "/api/v1/projects/{project_id}/federation/docbank"

func localAttachmentOrigin(ctx context.Context, cfg ServerConfig, origin string) (bool, error) {
	if origin == "" || origin == cfg.DB.InstanceUID() {
		return true, nil
	}
	former, err := cfg.DB.FormerInstanceUIDs(ctx)
	if err != nil {
		return false, internalAPIError(err)
	}
	return slices.Contains(former, origin), nil
}

// A disabled replica is still a replica. Never reinterpret it as a new local
// Docbank origin when an operator suspends its upstream transport.
func docbankUpstream(ctx context.Context, cfg ServerConfig, projectID int64) (*db.FederationBinding, error) {
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, internalAPIError(err)
	}
	if binding.Role == db.FederationRoleSpoke && !binding.Enabled {
		return nil, attachmentError(503, "federation_offline")
	}
	return &binding, nil
}

func docbankOriginCapabilities(ctx context.Context, cfg ServerConfig, project db.Project, binding *db.FederationBinding, origin string) (api.DocbankServiceCapabilities, error) {
	if binding == nil || binding.Role != db.FederationRoleSpoke {
		if cfg.Docbank == nil {
			return api.DocbankServiceCapabilities{}, attachmentError(503, "docbank_not_configured")
		}
		if origin == "" {
			origin = cfg.DB.InstanceUID()
		} else {
			local, err := localAttachmentOrigin(ctx, cfg, origin)
			if err != nil {
				return api.DocbankServiceCapabilities{}, err
			}
			if !local {
				return api.DocbankServiceCapabilities{}, attachmentError(409, "attachment_origin_mismatch")
			}
		}
		return api.DocbankServiceCapabilities{Version: 1, ProjectUID: project.UID, Origin: origin}, nil
	}
	query := url.Values{"project_uid": {project.UID}}
	if origin != "" {
		query.Set("origin", origin)
	}
	resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodGet, "capabilities", query, nil)
	if err != nil {
		return api.DocbankServiceCapabilities{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var caps api.DocbankServiceCapabilities
	if json.UnmarshalRead(io.LimitReader(resp.Body, 64<<10), &caps) != nil || caps.Version != 1 || caps.ProjectUID != project.UID || !katauid.Valid(caps.Origin) {
		return caps, attachmentError(503, "docbank_forward_unsupported")
	}
	if binding.RelayConfig != nil && caps.Origin != binding.RelayConfig.AuthorityUID {
		return caps, attachmentError(409, "attachment_origin_mismatch")
	}
	return caps, nil
}

// docbankForwardRequest uses the same daemon-owned, origin-pinned credential
// route as synchronous leases. It never copies an incoming Authorization.
func docbankForwardRequest(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, method, suffix string, query url.Values, body io.Reader) (*http.Response, error) {
	finish, err := beginDocbankForward(ctx, cfg, binding)
	if err != nil {
		return nil, err
	}
	defer finish()
	project, err := cfg.DB.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	if !found || binding.HubProjectUID != project.UID || credential.LeavePending || credential.NativeLeave != nil {
		return nil, attachmentError(503, "federation_offline")
	}
	remote, credential, err := newClaimForwardClient(ctx, binding, credential)
	if err != nil {
		return nil, attachmentError(503, "federation_offline")
	}
	if remote.transportErr != nil {
		return nil, attachmentError(503, "federation_offline")
	}
	if strings.HasSuffix(suffix, "/uploads") || strings.HasSuffix(suffix, "/content") {
		remote.client.Timeout = attachmentProxyTimeout
	}
	endpoint := remote.baseURL + "/api/v1/projects/" + strconv.FormatInt(credential.HubProjectID, 10) + "/federation/docbank/" + suffix
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, attachmentError(503, "federation_offline")
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		if strings.HasSuffix(suffix, "/uploads") {
			req.Header.Set("Content-Type", "application/octet-stream")
		}
	}
	req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	if err := addClaimForwardProjectScope(ctx, req); err != nil {
		return nil, err
	}
	resp, err := remote.client.Do(req) //nolint:gosec // G704: destination and credential are pinned to the project's configured upstream origin.
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, attachmentError(http.StatusRequestEntityTooLarge, "attachment_too_large")
		}
		return nil, attachmentError(503, "federation_offline")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound && suffix == "capabilities" {
		return nil, attachmentError(503, "docbank_forward_unsupported")
	}
	var envelope api.ErrorEnvelope
	if json.UnmarshalRead(io.LimitReader(resp.Body, 4096), &envelope) != nil || envelope.Error.Code == "" {
		if resp.StatusCode == http.StatusNotFound {
			return nil, attachmentError(503, "docbank_forward_unsupported")
		}
		return nil, attachmentError(502, "docbank_upstream_failed")
	}
	// Codes are stable API vocabulary; upstream prose can contain private data.
	code := envelope.Error.Code
	if strings.ContainsFunc(code, func(r rune) bool { return r != '_' && r != '.' && (r < 'a' || r > 'z') }) {
		code = "docbank_upstream_failed"
	}
	switch code {
	case "docbank_outside_scope":
		return nil, api.NewError(resp.StatusCode, code, "file is outside the configured Docbank folder", "move the file into the configured folder", nil)
	case "attachment_issue_not_synced":
		return nil, api.NewError(resp.StatusCode, code, "issue is not available at the upstream yet", "sync the project and retry", nil)
	case "claim_denied":
		return nil, api.NewError(resp.StatusCode, code, "lease denied for federated issue mutation", "run kata federation lease acquire <ref>", nil)
	}
	return nil, attachmentError(resp.StatusCode, code)
}

// beginDocbankForward pairs the current route and credential under the same
// gate used by rebinds, before any request can carry the credential.
func beginDocbankForward(ctx context.Context, cfg ServerConfig, binding db.FederationBinding) (func(), error) {
	finish, err := beginClaimFederationTransport(ctx, cfg, binding.ProjectID)
	if err != nil {
		return nil, err
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, binding.ProjectID)
	if err != nil || !sameClaimRefreshRoute(current, binding) {
		finish()
		return nil, attachmentError(http.StatusServiceUnavailable, "federation_offline")
	}
	return finish, nil
}

func sameClaimRefreshRoute(a, b db.FederationBinding) bool {
	if a.Role != b.Role || a.Enabled != b.Enabled || a.PushEnabled != b.PushEnabled || a.HubURL != b.HubURL || a.HubProjectID != b.HubProjectID || a.HubProjectUID != b.HubProjectUID || a.AllowInsecure != b.AllowInsecure {
		return false
	}
	if a.RelayConfig == nil {
		return b.RelayConfig == nil
	}
	return b.RelayConfig != nil && a.RelayConfig.BindingUID == b.RelayConfig.BindingUID && a.RelayConfig.AuthorityUID == b.RelayConfig.AuthorityUID
}

// ensureDocbankIssueUpstream sends pending issue events before a forwarded write
// only when the claim authority reports that it has not seen the issue yet.
func ensureDocbankIssueUpstream(ctx context.Context, cfg ServerConfig, project db.Project, binding db.FederationBinding, issue db.Issue, result *claimRefreshResult) error {
	if result != nil && result.known && !sameClaimRefreshRoute(result.binding, binding) {
		result = nil
	}
	if result != nil && result.known && !result.missing {
		return nil
	}
	credential, missing, err := func() (config.FederationCredential, bool, error) {
		finish, err := beginDocbankForward(ctx, cfg, binding)
		if err != nil {
			return config.FederationCredential{}, false, err
		}
		defer finish()
		remote, credential, err := claimForwardClient(ctx, cfg, binding)
		if err != nil {
			return credential, false, err
		}
		if result != nil && result.known {
			return credential, true, nil
		}
		_, err = remote.ClaimStatus(ctx, credential.HubProjectID, issue.UID)
		var status *claimHubStatusError
		if !errors.As(err, &status) || status == nil || status.StatusCode != http.StatusNotFound {
			return credential, false, nil
		}
		code := hubStatusErrorCode(status)
		return credential, code == "issue_not_found" || (binding.RelayConfig != nil && code == "hub_claim_failed"), nil
	}()
	if err != nil || !missing {
		return err
	}
	if cfg.FederationSyncOnce == nil {
		return attachmentError(http.StatusServiceUnavailable, "federation_offline")
	}
	syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := cfg.FederationSyncOnce(syncCtx, binding, credential); err != nil {
		return attachmentError(http.StatusServiceUnavailable, "federation_offline")
	}
	currentBinding, err := cfg.DB.FederationBindingByProject(ctx, project.ID)
	if err != nil || !sameClaimRefreshRoute(currentBinding, binding) {
		return attachmentError(http.StatusServiceUnavailable, "federation_offline")
	}
	current, err := activeIssueByRef(ctx, cfg.DB, project.ID, issue.UID, db.IncludeDeletedNo)
	if err != nil {
		return err
	}
	if current.ProjectID != project.ID {
		return projectAccessDenied()
	}
	return nil
}

func forwardDocbankDescriptor(ctx context.Context, cfg ServerConfig, project db.Project, binding *db.FederationBinding, issue db.Issue, result *claimRefreshResult, expectedOrigin, name string, input *api.LinkAttachmentInput, body io.Reader) (metadata.Attachment, error) {
	origin := expectedOrigin
	if origin == "" {
		caps, err := docbankOriginCapabilities(ctx, cfg, project, binding, "")
		if err != nil {
			return metadata.Attachment{}, err
		}
		origin = caps.Origin
	}
	if !binding.PushEnabled {
		return metadata.Attachment{}, federationReadOnlyError(db.ErrFederatedReadOnly)
	}
	if err := ensureDocbankIssueUpstream(ctx, cfg, project, *binding, issue, result); err != nil {
		return metadata.Attachment{}, err
	}
	query := url.Values{"project_uid": {project.UID}, "origin": {origin}}
	suffix := "issues/" + url.PathEscape(issue.UID)
	if input == nil {
		suffix += "/uploads"
		query.Set("name", name)
	} else {
		suffix += "/links"
		forwardInput := *input
		forwardInput.Actor, forwardInput.ReuseExisting = "", false
		encoded, err := json.Marshal(forwardInput)
		if err != nil {
			return metadata.Attachment{}, internalAPIError(err)
		}
		body = bytes.NewReader(encoded)
	}
	resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodPost, suffix, query, body)
	if err != nil {
		return metadata.Attachment{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var descriptor struct {
		ProjectUID string         `json:"project_uid"`
		Attachment jsontext.Value `json:"attachment"`
	}
	if json.UnmarshalRead(io.LimitReader(resp.Body, 64<<10), &descriptor) != nil || descriptor.ProjectUID != project.UID {
		return metadata.Attachment{}, attachmentError(502, "attachment_invalid")
	}
	attachment, valid := metadata.Valid(descriptor.Attachment)
	if !valid || attachment.Origin != origin {
		return metadata.Attachment{}, attachmentError(502, "attachment_invalid")
	}
	// A rebind during the transfer must not attach a descriptor from a former
	// upstream to this project's newly selected route.
	current, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return metadata.Attachment{}, err
	}
	if current == nil || !sameClaimRefreshRoute(*current, *binding) {
		return metadata.Attachment{}, attachmentError(409, "attachment_origin_mismatch")
	}
	return attachment, nil
}

func exposedDocbankNode(cfg ServerConfig, node docbank.Node) error {
	if node.TrashedAt != "" {
		return attachmentError(410, "attachment_content_missing")
	}
	folder := path.Clean("/" + strings.Trim(cfg.DocbankFolder, "/"))
	if node.Path == "" || !strings.HasPrefix(node.Path, "/") || path.Clean(node.Path) != node.Path || strings.ContainsAny(node.Path, "\\\x00") {
		return attachmentError(502, "docbank_scope_unavailable")
	}
	if folder != "/" && node.Path != folder && !strings.HasPrefix(node.Path, folder+"/") {
		return api.NewError(http.StatusForbidden, "docbank_outside_scope", "file is outside the configured Docbank folder", "move the file into the configured folder", nil)
	}
	return nil
}

// attachmentPatchHook decides the patch under the metadata write transaction.
// It rejects a write whose Docbank route changed since the handler read it, so
// a reference never commits with an origin from a stale binding. A reuse
// request also rechecks there, so concurrent retries cannot each observe an
// absent reference and create a second one.
func attachmentPatchHook(patch map[string]jsontext.Value, observed *db.FederationBinding, reuseVersion *uuid.UUID, origin string, localOrigins []string) db.MetadataPatchHook {
	return func(ctx context.Context, tx *sql.Tx, current db.Issue) (map[string]jsontext.Value, error) {
		var role, hubURL, hubProjectUID string
		err := tx.QueryRowContext(ctx, `SELECT role, hub_url, hub_project_uid FROM federation_bindings WHERE project_id=$1`, current.ProjectID).Scan(&role, &hubURL, &hubProjectUID)
		if observed == nil && !errors.Is(err, sql.ErrNoRows) || observed != nil && (err != nil || role != string(observed.Role) || hubURL != observed.HubURL || hubProjectUID != observed.HubProjectUID) {
			return nil, attachmentError(409, "attachment_route_changed")
		}
		if reuseVersion == nil {
			return patch, nil
		}
		existingID, _, err := attachmentForVersion(current, docbank.Version{ID: *reuseVersion}, origin, localOrigins)
		if err != nil || existingID != "" {
			return map[string]jsontext.Value{}, err
		}
		return patch, nil
	}
}
