package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

var attachmentUploadRoute = regexp.MustCompile(`^(/api/v1/ui/proxy)?/api/v1/projects/[^/]+/issues/[^/]+/attachments$`)

func isAttachmentUpload(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return r.Method == http.MethodPost && err == nil && mediaType == "multipart/form-data" && attachmentUploadRoute.MatchString(r.URL.Path)
}

const attachmentPath = "/api/v1/projects/{project_id}/issues/{ref}/attachments"

type attachmentUploadContextKey struct{}
type attachmentUploadPreflight struct {
	actor string
	issue db.Issue
}

func attachmentError(status int, code string) error {
	return api.NewError(status, code, strings.ReplaceAll(code, "_", " "), "", nil)
}
func docbankError(err error) error {
	switch docbank.ErrorKind(err) {
	case "foreign_vault":
		return attachmentError(409, "foreign_vault")
	case "not_found":
		return attachmentError(410, "attachment_content_missing")
	case "too_large":
		return attachmentError(413, "attachment_too_large")
	case "unauthorized":
		return attachmentError(502, "docbank_unauthorized")
	case "integrity", "invalid_receipt":
		return attachmentError(502, "attachment_integrity")
	default:
		return attachmentError(502, "docbank_unreachable")
	}
}
func registerAttachmentHandlers(h huma.API, cfg ServerConfig) {
	registerAttachmentReadHandlers(h, cfg)
	limit := cfg.MaxAttachmentBytes
	if limit <= 0 {
		limit = 256 << 20
	}
	folder := cfg.DocbankFolder
	if folder == "" {
		folder = "/kata"
	}
	slots := make(chan struct{}, 2)
	bound := func(ctx huma.Context, next func(huma.Context)) {
		var prepared attachmentUploadPreflight
		if err := func() error {
			if cfg.Docbank == nil {
				return attachmentError(503, "docbank_not_configured")
			}
			projectID, err := strconv.ParseInt(ctx.Param("project_id"), 10, 64)
			if err != nil {
				return attachmentError(400, "validation")
			}
			actor, err := attributedActor(ctx.Context(), ctx.Query("actor"))
			if err != nil {
				return err
			}
			iss, err := activeIssueByRef(ctx.Context(), cfg.DB, projectID, ctx.Param("ref"), db.IncludeDeletedNo)
			if err != nil {
				return err
			}
			prepared = attachmentUploadPreflight{actor: actor, issue: iss}
			return requireFederatedIssueClaim(ctx.Context(), cfg, projectID, iss, actor)
		}(); err != nil {
			err = internalAPIError(err)
			status := http.StatusInternalServerError
			if statusError, ok := errors.AsType[huma.StatusError](err); ok {
				status = statusError.GetStatus()
			}
			ctx.SetHeader("Content-Type", "application/json")
			ctx.SetStatus(status)
			_ = json.MarshalWrite(ctx.BodyWriter(), err)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			_ = huma.WriteErr(h, ctx, 429, "attachment upload concurrency limit")
			return
		}
		r, w := humago.Unwrap(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, limit+(64<<10))
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
			code := 400
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				code = 413
			}
			_ = huma.WriteErr(h, ctx, code, "invalid attachment multipart body")
			return
		}
		defer func() { _ = r.MultipartForm.RemoveAll() }()
		next(huma.WithContext(ctx, context.WithValue(ctx.Context(), attachmentUploadContextKey{}, prepared)))
	}
	huma.Register(h, huma.Operation{OperationID: "addAttachment", Method: "POST", Path: attachmentPath, BodyReadTimeout: -1, Middlewares: huma.Middlewares{bound}, RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"multipart/form-data": {Schema: &huma.Schema{Type: "object", Required: []string{"file", "attachment_id"}, Properties: map[string]*huma.Schema{"file": {Type: "string", Format: "binary"}, "attachment_id": {Type: "string"}}}}}}}, func(ctx context.Context, in *api.AddAttachmentRequest) (*api.AttachmentResponse, error) {
		prepared := ctx.Value(attachmentUploadContextKey{}).(attachmentUploadPreflight)
		actor, iss := prepared.actor, prepared.issue
		files := in.RawBody.File["file"]
		if len(files) != 1 || len(in.RawBody.File) != 1 || len(in.RawBody.Value["attachment_id"]) != 1 {
			return nil, attachmentError(400, "invalid_attachment_upload")
		}
		id := firstFormValue(in.RawBody.Value, "attachment_id")
		fh := files[0]
		if fh.Size > limit {
			return nil, attachmentError(413, "attachment_too_large")
		}
		name := metadata.CleanAttachmentName(fh.Filename)
		e := metadata.Attachment{Name: name, MediaType: "application/octet-stream", Size: fh.Size, AddedBy: actor, AddedAt: time.Now().UTC().Format(time.RFC3339)}
		// Validate the client id/name before contacting Docbank; reference fields follow the receipt.
		if !uid.Valid(id) || name == "" || len(name) > 255 {
			return nil, attachmentError(400, "attachment_invalid")
		}
		if err := preflightAttachmentAdmission(iss.Metadata, id); err != nil {
			return nil, err
		}
		file, err := fh.Open()
		if err != nil {
			return nil, internalAPIError(err)
		}
		defer func() { _ = file.Close() }()
		head := make([]byte, 512)
		n, err := io.ReadFull(file, head)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, internalAPIError(err)
		}
		e.MediaType = attachmentMediaType(head[:n])
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, internalAPIError(err)
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, file); err != nil {
			return nil, internalAPIError(err)
		}
		sha := hex.EncodeToString(hash.Sum(nil))
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, internalAPIError(err)
		}
		vault, err := cfg.Docbank.Info(ctx)
		if err != nil {
			return nil, docbankError(err)
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		parent, err := cfg.Docbank.EnsureFolder(ctx, path.Join(folder, project.UID))
		if err != nil {
			return nil, docbankError(err)
		}
		receipt, err := cfg.Docbank.Upload(ctx, parent, sha[:12]+"-"+name, e.MediaType, file, e.Size, sha)
		if err != nil {
			return nil, docbankError(err)
		}
		e.Docbank = metadata.DocbankReference{VaultUID: vault, NodeID: receipt.Node.ID, ContentVersionID: receipt.Node.VersionID, ContentSHA256: sha}
		saved, _, err := mutateAttachments(ctx, cfg, in.ProjectID, in.Ref, actor, func(m metadata.Attachments) (metadata.Attachment, bool, error) {
			if _, exists := m[id]; exists {
				old, err := m.Lookup(id)
				if err == nil && old.Name == e.Name && old.MediaType == e.MediaType && old.Size == e.Size && old.Docbank == e.Docbank {
					return old, false, nil
				}
				return e, false, attachmentError(409, "attachment_id_conflict")
			}
			if err := m.Add(id, e); err != nil {
				return e, false, attachmentError(400, "attachment_invalid")
			}
			return e, true, nil
		})
		if err != nil {
			return nil, err
		}
		out := &api.AttachmentResponse{}
		out.Body.AttachmentID = id
		out.Body.Attachment = saved
		return out, nil
	})
	huma.Register(h, huma.Operation{OperationID: "removeAttachment", Method: "DELETE", Path: attachmentPath + "/{attachment_id}"}, func(ctx context.Context, in *api.RemoveAttachmentRequest) (*api.RemoveAttachmentResponse, error) {
		actor, err := attributedActor(ctx, in.Actor)
		if err != nil {
			return nil, err
		}
		_, changed, err := mutateAttachments(ctx, cfg, in.ProjectID, in.Ref, actor, func(m metadata.Attachments) (metadata.Attachment, bool, error) {
			if _, ok := m[in.AttachmentID]; !ok {
				return metadata.Attachment{}, false, attachmentError(404, "attachment_not_found")
			}
			m.Remove(in.AttachmentID)
			return metadata.Attachment{}, true, nil
		})
		if err != nil {
			return nil, err
		}
		out := &api.RemoveAttachmentResponse{}
		out.Body.Changed = changed
		return out, nil
	})
}
func firstFormValue(m map[string][]string, key string) string {
	if len(m[key]) == 0 {
		return ""
	}
	return m[key][0]
}
func attachmentMediaType(head []byte) string {
	if len(head) >= 16 && string(head[4:8]) == "ftyp" {
		size := int(binary.BigEndian.Uint32(head[:4]))
		if size >= 16 && size <= len(head) && size%4 == 0 {
			for offset := 8; offset < size; offset += 4 {
				if offset == 12 {
					continue // Minor version, rather than a compatible brand.
				}
				switch string(head[offset : offset+4]) {
				case "qt  ":
					return "video/quicktime"
				case "isom", "iso2", "avc1", "mp41", "mp42":
					return "video/mp4"
				}
			}
		}
	}
	typ := http.DetectContentType(head)
	if typ == "application/ogg" {
		return "video/ogg"
	}
	return typ
}

// preflightAttachmentAdmission rejects an invalid map or a full issue before
// contacting Docbank. mutateAttachments repeats admission under its guarded
// metadata write because another writer can change the map during the upload.
func preflightAttachmentAdmission(raw db.JSONBlob, id string) error {
	var values map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return internalAPIError(err)
	}
	m, err := metadata.DecodeAttachments(values["attachments"])
	if err != nil {
		return attachmentError(400, "attachment_invalid")
	}
	if _, exists := m[id]; exists {
		if _, err := m.Lookup(id); err != nil {
			return attachmentError(400, "attachment_invalid")
		}
		return nil
	}
	count := 0
	for key := range m {
		if _, err := m.Lookup(key); err == nil {
			count++
		}
	}
	if count >= 100 {
		return attachmentError(400, "attachment_invalid")
	}
	return nil
}

// mutateAttachments reapplies only the intended add/remove after a guard conflict,
// preserving every unseen peer entry. No Docbank call occurs inside the retry loop.
func mutateAttachments(ctx context.Context, cfg ServerConfig, projectID int64, ref, actor string, edit func(metadata.Attachments) (metadata.Attachment, bool, error)) (metadata.Attachment, bool, error) {
	for range 8 {
		iss, err := activeIssueByRef(ctx, cfg.DB, projectID, ref, db.IncludeDeletedNo)
		if err != nil {
			return metadata.Attachment{}, false, err
		}
		if err := requireFederatedIssueClaim(ctx, cfg, projectID, iss, actor); err != nil {
			return metadata.Attachment{}, false, err
		}
		var values map[string]jsontext.Value
		if err := json.Unmarshal([]byte(iss.Metadata), &values); err != nil {
			return metadata.Attachment{}, false, internalAPIError(err)
		}
		raw := values["attachments"]
		m, err := metadata.DecodeAttachments(raw)
		if err != nil {
			return metadata.Attachment{}, false, attachmentError(400, "attachment_invalid")
		}
		saved, changed, err := edit(m)
		if err != nil || !changed {
			return saved, changed, err
		}
		guard := &db.MetadataPatchGuard{Key: "attachments", IfValue: raw}
		if len(raw) == 0 || string(raw) == "null" {
			guard.IfAbsent = true
			guard.IfValue = nil
		}
		res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: actor, Patch: map[string]jsontext.Value{"attachments": m.Encode()}, Guard: guard})
		if _, ok := errors.AsType[*db.MetadataGuardConflictError](err); ok {
			continue
		}
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return saved, false, federationReadOnlyError(err)
		}
		if err != nil {
			return saved, false, internalAPIError(err)
		}
		if res.Changed {
			// The response carries no event projection. Publish the retained
			// transaction event before any response-only read can fail.
			cfg.Publish().Event(res.Event.ProjectID, res.Event)
		}
		return saved, res.Changed, nil
	}
	return metadata.Attachment{}, false, attachmentError(409, "metadata_guard_failed")
}
