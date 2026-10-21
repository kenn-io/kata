package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/uid"
)

const attachmentPath = "/api/v1/projects/{project_id}/issues/{ref}/attachments"
const maxAttachmentBytes = 256 << 20

var attachmentUploadRoute = regexp.MustCompile(`^(/api/v1/ui/proxy)?/api/v1/projects/[^/]+/issues/[^/]+/attachments$`)

func isAttachmentUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && attachmentUploadRoute.MatchString(r.URL.Path)
}

type attachmentUploadContextKey struct{}

func attachmentError(status int, code string) error {
	return api.NewError(status, code, strings.ReplaceAll(code, "_", " "), "", nil)
}
func docbankError(err error) error {
	if e, ok := errors.AsType[*docbank.Error](err); ok {
		switch e.Status {
		case 404:
			return attachmentError(410, "attachment_content_missing")
		case 401, 403:
			return attachmentError(502, "docbank_unauthorized")
		}
	}
	return attachmentError(502, "docbank_unreachable")
}

// The nine extension entries work even on hosts without a MIME database.
var attachmentTypes = map[string]string{
	".png": "image/png", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mov": "video/quicktime", ".webm": "video/webm", ".ogv": "video/ogg",
}

func inlineAttachmentType(typ string) bool {
	for _, allowed := range attachmentTypes {
		if typ == allowed {
			return true
		}
	}
	return false
}
func registerAttachmentHandlers(h huma.API, cfg ServerConfig) {
	rawBody := func(ctx huma.Context, next func(huma.Context)) {
		r, w := humago.Unwrap(ctx)
		// Match huma's unlimited body-read timeout for a streaming upload.
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		next(huma.WithContext(ctx, context.WithValue(ctx.Context(), attachmentUploadContextKey{}, http.MaxBytesReader(w, r.Body, maxAttachmentBytes))))
	}
	huma.Register(h, huma.Operation{OperationID: "addAttachment", Method: "POST", Path: attachmentPath, Hidden: true, BodyReadTimeout: -1, Middlewares: huma.Middlewares{rawBody}}, func(ctx context.Context, in *api.AddAttachmentRequest) (*huma.StreamResponse, error) {
		if cfg.Docbank == nil {
			return nil, attachmentError(503, "docbank_not_configured")
		}
		actor, err := attributedActor(ctx, in.Actor)
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
		e := metadata.Attachment{Name: in.Name, MediaType: "application/octet-stream"}
		if e.Name == "" || len(e.Name) > 255 || path.Base(e.Name) != e.Name || strings.ContainsAny(e.Name, `\`) || e.Name == "." || e.Name == ".." || strings.ContainsFunc(e.Name, unicode.IsControl) {
			return nil, attachmentError(400, "attachment_invalid")
		}
		if typ := attachmentTypes[strings.ToLower(path.Ext(e.Name))]; typ != "" {
			e.MediaType = typ
		} else if typ := mime.TypeByExtension(strings.ToLower(path.Ext(e.Name))); inlineAttachmentType(typ) {
			e.MediaType = typ
		}
		file, err := os.CreateTemp("", "kata-attachment-*")
		if err != nil {
			return nil, internalAPIError(err)
		}
		defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
		hash := sha256.New()
		e.Size, err = io.Copy(io.MultiWriter(file, hash), ctx.Value(attachmentUploadContextKey{}).(io.Reader))
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, attachmentError(413, "attachment_too_large")
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return nil, internalAPIError(err)
		}
		project, err := cfg.DB.ProjectByID(ctx, in.ProjectID)
		if err != nil {
			return nil, internalAPIError(err)
		}
		folder := cfg.DocbankFolder
		if folder == "" {
			folder = "/kata"
		}
		parent, err := cfg.Docbank.EnsureFolder(ctx, path.Join(folder, project.UID))
		if err != nil {
			return nil, docbankError(err)
		}
		sha := hex.EncodeToString(hash.Sum(nil))
		e.DocbankVersion, err = cfg.Docbank.Upload(ctx, parent, sha[:12]+"-"+e.Name, file, e.Size, sha)
		if err != nil {
			return nil, docbankError(err)
		}
		id, err := uid.New()
		if err != nil {
			return nil, internalAPIError(err)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, internalAPIError(err)
		}
		res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: actor, Patch: map[string]jsontext.Value{metadata.AttachmentPrefix + id: raw}})
		if errors.Is(err, db.ErrFederatedReadOnly) {
			return nil, federationReadOnlyError(err)
		}
		if err != nil {
			return nil, internalAPIError(err)
		}
		if res.Changed {
			cfg.Publish().Event(res.Event.ProjectID, res.Event)
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", "application/json")
			_ = json.MarshalWrite(hctx.BodyWriter(), struct {
				ID         string              `json:"attachment_id"`
				Attachment metadata.Attachment `json:"attachment"`
			}{id, e})
		}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "attachmentContent", Method: "GET", Path: attachmentPath + "/{attachment_id}/content", Hidden: true}, func(ctx context.Context, in *api.AttachmentReadRequest) (*huma.StreamResponse, error) {
		iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
		if err != nil {
			return nil, err
		}
		var values map[string]jsontext.Value
		if err = json.Unmarshal([]byte(iss.Metadata), &values); err != nil {
			return nil, internalAPIError(err)
		}
		raw, exists := values[metadata.AttachmentPrefix+in.AttachmentID]
		if !exists {
			return nil, attachmentError(404, "attachment_not_found")
		}
		e, valid := metadata.Valid(raw)
		if !valid {
			return nil, attachmentError(400, "attachment_invalid")
		}
		if cfg.Docbank == nil {
			return nil, attachmentError(503, "docbank_not_configured")
		}
		resp, err := cfg.Docbank.Content(ctx, e.DocbankVersion)
		if err != nil {
			return nil, docbankError(err)
		}
		typ, disposition := "application/octet-stream", "attachment"
		if inlineAttachmentType(e.MediaType) {
			typ, disposition = e.MediaType, "inline"
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = resp.Body.Close() }()
			hctx.SetHeader("Content-Type", typ)
			hctx.SetHeader("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": e.Name}))
			if resp.ContentLength >= 0 {
				hctx.SetHeader("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
			}
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			if _, err := io.Copy(hctx.BodyWriter(), resp.Body); err != nil {
				panic(http.ErrAbortHandler)
			}
		}}, nil
	})
}
