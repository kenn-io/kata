package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

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
	if cfg.Docbank == nil {
		return nil, attachmentError(503, "docbank_not_configured")
	}
	actor, err := attributedActor(ctx, in.Body.Actor)
	if err != nil {
		return nil, err
	}
	iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
	if err != nil {
		return nil, err
	}
	if err := requireFederatedIssueClaim(ctx, cfg, in.ProjectID, iss, actor); err != nil {
		return nil, err
	}
	input := in.Body
	if (input.NodeID == nil) == (input.Path == nil) || (input.NodeID != nil && *input.NodeID <= 0) {
		return nil, attachmentError(400, "attachment_invalid")
	}
	var node docbank.Node
	if input.Path != nil {
		node, err = cfg.Docbank.ResolvePath(ctx, *input.Path)
	} else {
		node, err = cfg.Docbank.Node(ctx, *input.NodeID)
	}
	if err != nil {
		return nil, docbankError(err, docbankLinkErrors)
	}
	if node.TrashedAt != "" {
		return nil, attachmentError(410, "attachment_content_missing")
	}
	if node.Kind != "file" {
		return nil, attachmentError(400, "attachment_not_file")
	}
	version := docbank.Version{ID: node.CurrentVersionID, NodeID: node.ID, Size: node.Size}
	if input.VersionID != nil {
		version, err = cfg.Docbank.Version(ctx, *input.VersionID)
		if err != nil {
			return nil, docbankError(err, docbankLinkErrors)
		}
	}
	if version.NodeID != node.ID {
		return nil, attachmentError(400, "attachment_version_mismatch")
	}
	if !metadata.ValidAttachmentName(node.Name) {
		return nil, attachmentError(400, "attachment_invalid")
	}
	attachment := metadata.Attachment{Name: node.Name, Size: version.Size, DocbankVersion: version.ID}
	if input.ReuseExisting {
		return saveAttachment(ctx, cfg, in.ProjectID, iss, actor, attachment, &version.ID)
	}
	return saveAttachment(ctx, cfg, in.ProjectID, iss, actor, attachment, nil)
}

func attachmentForVersion(iss db.Issue, version docbank.Version) (string, metadata.Attachment, error) {
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
		if !valid || attachment.DocbankVersion != version.ID {
			continue
		}
		if bestID == "" || id < bestID {
			bestID, best = id, attachment
		}
	}
	return bestID, best, nil
}
