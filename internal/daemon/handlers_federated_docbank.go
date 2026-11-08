package daemon

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
)

func authorizeDocbankService(ctx context.Context, cfg ServerConfig, in api.DocbankServiceRequest, capabilities, operation string) (context.Context, db.Project, federationPrincipal, error) {
	ctx, principal, err := authorizeFederationRequestForCapabilities(ctx, cfg, in.Authorization, in.ProjectID, strings.Split(capabilities, ","), federationTransportOperation(operation))
	if err != nil {
		return ctx, db.Project{}, principal, err
	}
	project, err := activeProjectByID(ctx, cfg.DB, in.ProjectID)
	if err != nil {
		return ctx, project, principal, err
	}
	if project.UID != in.ProjectUID {
		return ctx, project, principal, projectAccessDenied()
	}
	return ctx, project, principal, nil
}

func docbankServiceIssue(ctx context.Context, cfg ServerConfig, project db.Project, ref string, principal db.ClaimPrincipal) (db.Issue, error) {
	issue, err := activeIssueByRef(ctx, cfg.DB, project.ID, ref, db.IncludeDeletedNo)
	if err != nil {
		if apiErr, ok := err.(*api.APIError); ok && apiErr.Status == http.StatusNotFound {
			return issue, api.NewError(503, "attachment_issue_not_synced", "issue is not available at the upstream yet", "sync the project and retry", nil)
		}
		return issue, err
	}
	if issue.ProjectID != project.ID {
		return issue, projectAccessDenied()
	}
	return issue, checkFederatedIssueClaim(ctx, cfg, issue, principal, false, true)
}

func registerFederatedDocbankHandlers(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "federatedDocbankCapabilities", Method: http.MethodGet, Path: federatedDocbankPath + "/capabilities"}, func(ctx context.Context, in *api.DocbankServiceRequest) (*api.DocbankServiceCapabilitiesResponse, error) {
		ctx, project, _, err := authorizeDocbankService(ctx, cfg, *in, "pull,push", "federatedDocbankCapabilities")
		if err != nil {
			return nil, err
		}
		caps, err := docbankOriginCapabilities(ctx, cfg, project)
		if err != nil {
			return nil, err
		}
		return &api.DocbankServiceCapabilitiesResponse{Body: caps}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "uploadFederatedDocbank", Method: http.MethodPost, Path: federatedDocbankPath + "/issues/{ref}/uploads", RequestBody: &huma.RequestBody{Required: true, Content: binaryContent("application/octet-stream")}, BodyReadTimeout: -1, Middlewares: huma.Middlewares{attachmentRawBody}}, func(ctx context.Context, in *api.DocbankUploadRequest) (*api.DocbankDescriptorResponse, error) {
		ctx, project, fed, err := authorizeDocbankService(ctx, cfg, in.DocbankServiceRequest, "push", "uploadFederatedDocbank")
		if err != nil {
			return nil, err
		}
		principal := db.ClaimPrincipal{HolderInstanceUID: fed.SpokeInstanceUID, Holder: fed.Actor, ClientKind: in.ClientKind}
		issue, err := docbankServiceIssue(ctx, cfg, project, in.Ref, principal)
		if err != nil {
			return nil, err
		}
		if !metadata.ValidAttachmentName(in.Name) {
			return nil, attachmentError(400, "attachment_invalid")
		}
		recheck := func() error {
			// Force a live enrollment decision after a lengthy body transfer;
			// the middleware's cached request authorization is not enough here.
			authorization, err := evaluateFederationRequest(ctx, cfg, in.Authorization, project.ID, "push", federationTransportOperation("uploadFederatedDocbank"))
			if err != nil {
				return err
			}
			_, err = docbankServiceIssue(db.WithAdditionalTransactionFence(ctx, authorization.transactionFence), cfg, project, issue.UID, principal)
			return err
		}
		caps, err := docbankOriginCapabilities(ctx, cfg, project)
		if err != nil {
			return nil, err
		}
		if caps.Origin != in.Origin {
			return nil, attachmentError(409, "attachment_origin_mismatch")
		}
		binding, err := docbankUpstream(ctx, cfg, project.ID)
		if err != nil {
			return nil, err
		}
		var descriptor metadata.Attachment
		if binding != nil {
			descriptor, err = forwardDocbankDescriptor(ctx, cfg, project, issue, boundSpokeClaimPrincipal(*binding, principal, cfg.DB.InstanceUID()), in.Origin, in.Name, nil, ctx.Value(attachmentUploadContextKey{}).(io.Reader))
		} else {
			descriptor, err = uploadDocbankAttachment(ctx, cfg, issue, in.Name, ctx.Value(attachmentUploadContextKey{}).(io.Reader), recheck)
			descriptor.Origin = cfg.DB.InstanceUID()
		}
		if err != nil {
			return nil, err
		}
		if err := recheck(); err != nil {
			return nil, err
		}
		return &api.DocbankDescriptorResponse{Body: api.DocbankDescriptor{ProjectUID: project.UID, Attachment: descriptor}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "linkFederatedDocbank", Method: http.MethodPost, Path: federatedDocbankPath + "/issues/{ref}/links"}, func(ctx context.Context, in *api.DocbankLinkRequest) (*api.DocbankDescriptorResponse, error) {
		ctx, project, fed, err := authorizeDocbankService(ctx, cfg, in.DocbankServiceRequest, "push", "linkFederatedDocbank")
		if err != nil {
			return nil, err
		}
		principal := db.ClaimPrincipal{HolderInstanceUID: fed.SpokeInstanceUID, Holder: fed.Actor, ClientKind: in.ClientKind}
		issue, err := docbankServiceIssue(ctx, cfg, project, in.Ref, principal)
		if err != nil {
			return nil, err
		}
		caps, err := docbankOriginCapabilities(ctx, cfg, project)
		if err != nil {
			return nil, err
		}
		if caps.Origin != in.Origin {
			return nil, attachmentError(409, "attachment_origin_mismatch")
		}
		binding, err := docbankUpstream(ctx, cfg, project.ID)
		if err != nil {
			return nil, err
		}
		var descriptor metadata.Attachment
		if binding != nil {
			descriptor, err = forwardDocbankDescriptor(ctx, cfg, project, issue, boundSpokeClaimPrincipal(*binding, principal, cfg.DB.InstanceUID()), in.Origin, "", &in.Body, nil)
		} else {
			descriptor, err = resolveDocbankAttachment(ctx, cfg, in.Body, true)
			descriptor.Origin = cfg.DB.InstanceUID()
		}
		if err != nil {
			return nil, err
		}
		authorization, err := evaluateFederationRequest(ctx, cfg, in.Authorization, project.ID, "push", federationTransportOperation("linkFederatedDocbank"))
		if err != nil {
			return nil, err
		}
		if _, err := docbankServiceIssue(db.WithAdditionalTransactionFence(ctx, authorization.transactionFence), cfg, project, issue.UID, principal); err != nil {
			return nil, err
		}
		return &api.DocbankDescriptorResponse{Body: api.DocbankDescriptor{ProjectUID: project.UID, Attachment: descriptor}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "federatedDocbankContent", Method: http.MethodGet, Path: federatedDocbankPath + "/versions/{version_id}/content", Responses: map[string]*huma.Response{"200": {Description: "Origin Docbank content.", Content: binaryContent("*/*")}}}, func(ctx context.Context, in *api.DocbankContentRequest) (*huma.StreamResponse, error) {
		ctx, project, _, err := authorizeDocbankService(ctx, cfg, in.DocbankServiceRequest, "pull", "federatedDocbankContent")
		if err != nil {
			return nil, err
		}
		e := metadata.Attachment{Origin: in.Origin, DocbankVersion: in.VersionID}
		resp, err := originDocbankContent(ctx, cfg, project, e)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = resp.Body.Close() }()
			for _, name := range []string{"X-Docbank-Blob-Hash", "X-Docbank-Blob-Size"} {
				hctx.SetHeader(name, resp.Header.Get(name))
			}
			hctx.SetHeader("Content-Type", "application/octet-stream")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			if _, err := io.Copy(hctx.BodyWriter(), resp.Body); err != nil {
				panic(http.ErrAbortHandler)
			}
		}}, nil
	})
}

func originDocbankContent(ctx context.Context, cfg ServerConfig, project db.Project, attachment metadata.Attachment) (*http.Response, error) {
	binding, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return nil, err
	}
	if binding == nil {
		// Only projects without an upstream may use this daemon's Docbank origin.
		if attachment.Origin != cfg.DB.InstanceUID() {
			return nil, attachmentError(410, "attachment_origin_unavailable")
		}
		if cfg.Docbank == nil {
			return nil, attachmentError(503, "docbank_not_configured")
		}
		version, err := cfg.Docbank.Version(ctx, attachment.DocbankVersion)
		if err != nil {
			return nil, docbankError(err, docbankContentErrors)
		}
		node, err := cfg.Docbank.Node(ctx, version.NodeID)
		if err != nil {
			return nil, docbankError(err, docbankContentErrors)
		}
		if err := exposedDocbankNode(cfg, node); err != nil {
			return nil, err
		}
		resp, err := cfg.Docbank.Content(ctx, attachment.DocbankVersion)
		if err != nil {
			return nil, docbankError(err, docbankContentErrors)
		}
		return resp, nil
	}
	caps, err := docbankOriginCapabilities(ctx, cfg, project)
	if err != nil {
		return nil, err
	}
	if caps.Origin != attachment.Origin {
		return nil, attachmentError(409, "attachment_origin_mismatch")
	}
	resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodGet, "versions/"+attachment.DocbankVersion.String()+"/content", url.Values{"project_uid": {project.UID}, "origin": {attachment.Origin}}, nil)
	if err != nil {
		return nil, err
	}
	return docbank.VerifyContentResponse(resp)
}
