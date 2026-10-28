package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

func registerAttachmentLinkHandler(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "linkAttachment", Method: http.MethodPost, Path: attachmentPath + "/links", Errors: []int{400, 401, 403, 404, 409, 410, 502, 503}}, func(ctx context.Context, in *api.LinkAttachmentRequest) (*api.AddAttachmentResponse, error) {
		ctx, actor, err := attachmentWriteContext(ctx, in.Body.Actor)
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
		if (input.NodeID == nil) == (input.Path == nil) || (input.NodeID != nil && *input.NodeID <= 0) || (input.Path != nil && !strings.HasPrefix(*input.Path, "/")) {
			return nil, attachmentError(400, "attachment_invalid")
		}
		var versionID uuid.UUID
		if input.VersionID != nil {
			versionID, err = uuid.Parse(*input.VersionID)
			if err != nil || versionID == uuid.Nil() {
				return nil, attachmentError(400, "attachment_invalid")
			}
		}
		if cfg.Docbank == nil {
			return nil, attachmentError(503, "docbank_not_configured")
		}
		var node docbank.Node
		if input.NodeID != nil {
			node, err = cfg.Docbank.Node(ctx, *input.NodeID)
		} else {
			node, err = cfg.Docbank.ResolvePath(ctx, *input.Path)
		}
		if err != nil {
			return nil, docbankError(err, docbankContentErrors)
		}
		if node.TrashedAt != "" {
			return nil, attachmentError(410, "attachment_content_missing")
		}
		if node.Kind != "file" {
			return nil, attachmentError(400, "attachment_not_file")
		}
		if versionID == uuid.Nil() {
			versionID = node.CurrentVersionID
		}
		version, err := cfg.Docbank.Version(ctx, versionID)
		if err != nil {
			return nil, docbankError(err, docbankContentErrors)
		}
		if version.NodeID != node.ID {
			return nil, attachmentError(400, "attachment_version_mismatch")
		}
		entry := metadata.Attachment{Name: node.Name, MediaType: version.MediaType, Size: version.Size, DocbankVersion: version.ID}
		if entry.MediaType == "" {
			entry.MediaType = "application/octet-stream"
		}
		if !metadata.ValidAttachmentName(entry.Name) {
			return nil, attachmentError(400, "attachment_invalid")
		}
		id, err := uid.New()
		if err != nil {
			return nil, internalAPIError(err)
		}
		if err := recheckAttachmentWrite(ctx, cfg, in.ProjectID, iss, actor); err != nil {
			return nil, err
		}
		out := &api.AddAttachmentResponse{}
		ctx = db.WithMetadataPatchHook(ctx, func(_ context.Context, _ *sql.Tx, current db.Issue) (map[string]jsontext.Value, error) {
			if current.ProjectID != in.ProjectID {
				return nil, db.ErrIssueProjectChanged
			}
			var patch map[string]jsontext.Value
			out.Body, patch, err = attachmentLinkPatch(jsontext.Value(current.Metadata), id, entry)
			return patch, err
		})
		res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: actor})
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if errors.Is(err, db.ErrIssueProjectChanged) {
			return nil, attachmentError(409, "issue_project_changed")
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		if res.Changed {
			cfg.Publish().Event(res.Event.ProjectID, res.Event)
		}
		return out, nil
	})
}

// attachmentLinkPatch preserves every existing key and reuses the smallest live
// id for this version. Its caller supplies a fresh generated id and runs it under
// the metadata mutation lock.
func attachmentLinkPatch(current jsontext.Value, id string, entry metadata.Attachment) (api.AddAttachmentResponseBody, map[string]jsontext.Value, error) {
	var values map[string]jsontext.Value
	if err := json.Unmarshal(current, &values); err != nil {
		return api.AddAttachmentResponseBody{}, nil, err
	}
	out := api.AddAttachmentResponseBody{ID: id, Attachment: entry}
	found := ""
	for key, raw := range values {
		candidate, ok := strings.CutPrefix(key, metadata.AttachmentPrefix)
		if !ok || candidate == "" {
			continue
		}
		old, valid := metadata.Valid(raw)
		if valid && old.DocbankVersion == entry.DocbankVersion && (found == "" || candidate < found) {
			found = candidate
			out = api.AddAttachmentResponseBody{ID: candidate, Attachment: old}
		}
	}
	if found != "" {
		return out, nil, nil
	}
	raw, err := json.Marshal(entry)
	return out, map[string]jsontext.Value{metadata.AttachmentPrefix + id: raw}, err
}
