package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"path"
	"strings"
	"uuid"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
)

var docbankLinkErrors = map[int]error{
	401: attachmentError(502, "docbank_unauthorized"),
	403: attachmentError(502, "docbank_unauthorized"),
	404: api.NewError(404, "docbank_not_found", "Docbank file or version not found", "", nil),
}

func linkAttachment(ctx context.Context, cfg ServerConfig, in *api.LinkAttachmentRequest) (*api.AddAttachmentResponse, error) {
	actor, err := attributedActor(ctx, in.Body.Actor)
	if err != nil {
		return nil, err
	}
	iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
	if err != nil {
		return nil, err
	}
	if err := requireFederatedIssueClaimWithRefresh(ctx, cfg, iss, actor, true); err != nil {
		return nil, err
	}
	input := in.Body
	binding, err := docbankUpstream(ctx, cfg, iss.ProjectID)
	if err != nil {
		return nil, err
	}
	var attachment metadata.Attachment
	if binding != nil {
		project, err := activeProjectByID(ctx, cfg.DB, iss.ProjectID)
		if err != nil {
			return nil, err
		}
		principal, err := attachmentForwardPrincipal(ctx, cfg, *binding, iss, actor, in.Body.ClientKind)
		if err != nil {
			return nil, err
		}
		attachment, err = forwardDocbankDescriptor(ctx, cfg, project, iss, principal, "", "", &input, nil)
		if err != nil {
			return nil, err
		}
	} else {
		root, rootErr := cfg.DB.FederationBindingByProject(ctx, iss.ProjectID)
		if rootErr != nil && !errors.Is(rootErr, db.ErrNotFound) {
			return nil, internalAPIError(rootErr)
		}
		scoped := rootErr == nil && root.Enabled && root.Role == db.FederationRoleHub
		attachment, err = resolveDocbankAttachment(ctx, cfg, input, scoped)
		if err != nil {
			return nil, err
		}
		if scoped {
			attachment.Origin = cfg.DB.InstanceUID()
		}
	}
	if input.ReuseExisting {
		return saveAttachment(ctx, cfg, in.ProjectID, iss, actor, attachment, &attachment.DocbankVersion)
	}
	return saveAttachment(ctx, cfg, in.ProjectID, iss, actor, attachment, nil)
}

func attachmentForVersion(iss db.Issue, version docbank.Version, origin string) (string, metadata.Attachment, error) {
	var values map[string]jsontext.Value
	if err := json.Unmarshal([]byte(iss.Metadata), &values); err != nil {
		return "", metadata.Attachment{}, err
	}
	var bestID string
	var best metadata.Attachment
	for key, raw := range values {
		id, ok := strings.CutPrefix(key, metadata.AttachmentPrefix)
		if !ok || id == "" {
			continue
		}
		attachment, valid := metadata.Valid(raw)
		if !valid || attachment.DocbankVersion != version.ID || normalizedAttachmentOrigin(attachment.Origin) != normalizedAttachmentOrigin(origin) {
			continue
		}
		if bestID == "" || id < bestID {
			bestID, best = id, attachment
		}
	}
	return bestID, best, nil
}

func normalizedAttachmentOrigin(origin string) string {
	if origin == "local" {
		return ""
	}
	return origin
}

func attachmentMediaType(name string) string {
	if typ := attachmentTypes[strings.ToLower(path.Ext(name))]; typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func resolveDocbankAttachment(ctx context.Context, cfg ServerConfig, input api.LinkAttachmentInput, scoped bool) (metadata.Attachment, error) {
	if cfg.Docbank == nil {
		return metadata.Attachment{}, attachmentError(503, "docbank_not_configured")
	}
	var err error
	if (input.NodeID == nil) == (input.Path == nil) || (input.NodeID != nil && *input.NodeID <= 0) {
		return metadata.Attachment{}, attachmentError(400, "attachment_invalid")
	}
	var node docbank.Node
	if input.Path != nil {
		node, err = cfg.Docbank.ResolvePath(ctx, *input.Path)
	} else {
		node, err = cfg.Docbank.Node(ctx, *input.NodeID)
	}
	if err != nil {
		return metadata.Attachment{}, docbankError(err, docbankLinkErrors)
	}
	if scoped {
		if err := exposedDocbankNode(cfg, node); err != nil {
			return metadata.Attachment{}, err
		}
	}
	if node.TrashedAt != "" {
		return metadata.Attachment{}, attachmentError(410, "attachment_content_missing")
	}
	if node.Kind != "file" {
		return metadata.Attachment{}, attachmentError(400, "attachment_not_file")
	}
	version := docbank.Version{ID: node.CurrentVersionID, NodeID: node.ID, Size: node.Size}
	if input.VersionID != nil {
		version, err = cfg.Docbank.Version(ctx, *input.VersionID)
		if err != nil {
			return metadata.Attachment{}, docbankError(err, docbankLinkErrors)
		}
	}
	if version.NodeID != node.ID {
		return metadata.Attachment{}, attachmentError(400, "attachment_version_mismatch")
	}
	if !metadata.ValidAttachmentName(node.Name) {
		return metadata.Attachment{}, attachmentError(400, "attachment_invalid")
	}
	attachment := metadata.Attachment{Name: node.Name, MediaType: attachmentMediaType(node.Name), Size: version.Size, DocbankVersion: version.ID}
	if attachment.DocbankVersion == uuid.Nil() || attachment.Size < 0 {
		return metadata.Attachment{}, attachmentError(400, "attachment_invalid")
	}
	return attachment, nil
}
