package daemon

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/httpurl"
	"go.kenn.io/kata/internal/metadata"
	katauid "go.kenn.io/kata/internal/uid"
)

const federatedDocbankPath = "/api/v1/projects/{project_id}/federation/docbank"

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
	if binding.Role != db.FederationRoleSpoke {
		return nil, nil
	}
	if !binding.Enabled {
		return nil, attachmentError(503, "federation_offline")
	}
	return &binding, nil
}

func docbankOriginCapabilities(ctx context.Context, cfg ServerConfig, project db.Project) (api.DocbankServiceCapabilities, error) {
	binding, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return api.DocbankServiceCapabilities{}, err
	}
	if binding == nil {
		if cfg.Docbank == nil {
			return api.DocbankServiceCapabilities{}, attachmentError(503, "docbank_not_configured")
		}
		return api.DocbankServiceCapabilities{Version: 1, ProjectUID: project.UID, Origin: cfg.DB.InstanceUID()}, nil
	}
	resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodGet, "capabilities", url.Values{"project_uid": {project.UID}}, nil)
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
	project, err := cfg.DB.ProjectByID(ctx, binding.ProjectID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	credential, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil {
		return nil, internalAPIError(err)
	}
	storedURL, storedErr := httpurl.CanonicalHTTPBaseURL(credential.HubURL)
	boundURL, boundErr := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if !found || storedErr != nil || boundErr != nil || storedURL != boundURL || credential.HubProjectID != binding.HubProjectID || binding.HubProjectUID != project.UID || credential.LeavePending || credential.NativeLeave != nil {
		return nil, attachmentError(503, "federation_offline")
	}
	remote, credential, err := claimForwardClientWithCredential(ctx, binding, credential)
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
	return nil, attachmentError(resp.StatusCode, code)
}

// attachmentForwardPrincipal is used only by a local initiating request. The
// cache holds the already-bound lease tuple; hashing it again would change its
// identity. Ordinary local clients may switch labels while retaining ownership.
// Forwarded service requests never use this cache to select their principal.
func attachmentForwardPrincipal(ctx context.Context, cfg ServerConfig, binding db.FederationBinding, issue db.Issue, actor, clientKind string) (db.ClaimPrincipal, error) {
	principal := db.ClaimPrincipal{HolderInstanceUID: cfg.DB.InstanceUID(), Holder: actor, ClientKind: clientKind}
	if cfg.HostAccess != nil {
		requestPrincipal, ok := PrincipalFromContext(ctx)
		if !ok || !validHostPrincipal(requestPrincipal) {
			return db.ClaimPrincipal{}, attachmentError(401, "auth_required")
		}
		principal = hostClaimPrincipal(cfg, api.ClaimActionBody{}, requestPrincipal.Subject).ClaimPrincipal
	}
	principal = boundSpokeClaimPrincipal(binding, principal, cfg.DB.InstanceUID())
	if principal.AuthenticatedHost {
		return principal, nil
	}
	cached, err := cfg.DB.ClaimStatusReadOnly(ctx, issue.ProjectID, issue.UID, time.Now().UTC())
	if err != nil {
		return db.ClaimPrincipal{}, claimAPIError(err)
	}
	if cached.Held && cached.Claim != nil && db.SameClaimGateHolder(*cached.Claim, principal) {
		principal.ClientKind = cached.Claim.ClientKind
	}
	return principal, nil
}

func forwardDocbankDescriptor(ctx context.Context, cfg ServerConfig, project db.Project, issue db.Issue, forwarded db.ClaimPrincipal, expectedOrigin, name string, input *api.LinkAttachmentInput, body io.Reader) (metadata.Attachment, error) {
	caps, err := docbankOriginCapabilities(ctx, cfg, project)
	if err != nil {
		return metadata.Attachment{}, err
	}
	if expectedOrigin != "" && caps.Origin != expectedOrigin {
		return metadata.Attachment{}, attachmentError(409, "attachment_origin_mismatch")
	}
	binding, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return metadata.Attachment{}, err
	}
	if binding == nil {
		return metadata.Attachment{}, attachmentError(409, "attachment_origin_mismatch")
	}
	if !binding.PushEnabled {
		return metadata.Attachment{}, federationReadOnlyError(db.ErrFederatedReadOnly)
	}
	query := url.Values{"project_uid": {project.UID}, "client_kind": {forwarded.ClientKind}, "origin": {caps.Origin}}
	suffix := "issues/" + url.PathEscape(issue.UID)
	if input == nil {
		suffix += "/uploads"
		query.Set("name", name)
	} else {
		suffix += "/links"
		forwardInput := *input
		forwardInput.Actor, forwardInput.ClientKind, forwardInput.ReuseExisting = "", "", false
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
	var descriptor api.DocbankDescriptor
	if json.UnmarshalRead(io.LimitReader(resp.Body, 64<<10), &descriptor) != nil || descriptor.ProjectUID != project.UID || descriptor.Attachment.Origin != caps.Origin {
		return metadata.Attachment{}, attachmentError(502, "attachment_invalid")
	}
	raw, err := json.Marshal(descriptor.Attachment)
	if err != nil {
		return metadata.Attachment{}, internalAPIError(err)
	}
	if _, valid := metadata.Valid(raw); !valid {
		return metadata.Attachment{}, attachmentError(502, "attachment_invalid")
	}
	// A rebind during the transfer must not attach a descriptor from a former
	// upstream to this project's newly selected route.
	current, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return metadata.Attachment{}, err
	}
	if current == nil || current.HubURL != binding.HubURL || current.HubProjectUID != binding.HubProjectUID || current.HubProjectID != binding.HubProjectID {
		return metadata.Attachment{}, attachmentError(409, "attachment_origin_mismatch")
	}
	return descriptor.Attachment, nil
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
		return attachmentError(403, "docbank_outside_scope")
	}
	return nil
}
