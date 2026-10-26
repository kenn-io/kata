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

// attachmentProxyTimeout covers both sequential legs of a maximum-size transfer through the daemon switcher.
const attachmentProxyTimeout = 75 * time.Minute

var attachmentRoute = regexp.MustCompile(`^(?:/api/v1/ui/proxy)?/api/v1/projects/[^/]+/issues/[^/]+/attachments(/[^/]+/content)?$`)

// isAttachmentTransfer matches the upload and content routes, which stream file bytes.
func isAttachmentTransfer(r *http.Request) bool {
	m := attachmentRoute.FindStringSubmatch(r.URL.Path)
	return m != nil && (r.Method == http.MethodPost && m[1] == "" || r.Method == http.MethodGet && m[1] != "")
}

func isAttachmentUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && isAttachmentTransfer(r)
}

type attachmentUploadContextKey struct{}

func attachmentError(status int, code string) error {
	return api.NewError(status, code, strings.ReplaceAll(code, "_", " "), "", nil)
}

// Only a content read treats 404 as missing content; on upload it is a bad request.
var docbankUploadErrors = map[int]error{
	401: attachmentError(502, "docbank_unauthorized"),
	403: attachmentError(502, "docbank_unauthorized"),
	413: attachmentError(413, "attachment_too_large"),
}

var docbankContentErrors = map[int]error{
	401: attachmentError(502, "docbank_unauthorized"),
	403: attachmentError(502, "docbank_unauthorized"),
	404: attachmentError(410, "attachment_content_missing"),
}

func docbankError(err error, statusErrors map[int]error) error {
	if e, ok := errors.AsType[*docbank.Error](err); ok {
		if mapped := statusErrors[e.Status]; mapped != nil {
			return mapped
		}
		if e.Status >= 400 && e.Status < 500 {
			return attachmentError(400, "attachment_invalid")
		}
	}
	return attachmentError(502, "docbank_unreachable")
}

// The ten extension entries work even on hosts without a MIME database.
var attachmentTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
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

// binaryContent documents a raw byte stream, which huma cannot infer from a stream handler.
func binaryContent(mediaType string) map[string]*huma.MediaType {
	return map[string]*huma.MediaType{mediaType: {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}}
}

// recheckAttachmentWrite repeats the active-project and claim gates after the
// Docbank transfer, which can outlast an archive or a lost claim.
func recheckAttachmentWrite(ctx context.Context, cfg ServerConfig, projectID int64, iss db.Issue, actor string) error {
	if _, err := activeProjectByID(ctx, cfg.DB, projectID); err != nil {
		return err
	}
	return requireFederatedIssueClaim(ctx, cfg, projectID, iss, actor)
}

func registerAttachmentHandlers(h huma.API, cfg ServerConfig) {
	rawBody := func(ctx huma.Context, next func(huma.Context)) {
		r, w := humago.Unwrap(ctx)
		// Match huma's unlimited body-read timeout for a streaming upload.
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		next(huma.WithContext(ctx, context.WithValue(ctx.Context(), attachmentUploadContextKey{}, http.MaxBytesReader(w, r.Body, maxAttachmentBytes))))
	}
	huma.Register(h, huma.Operation{OperationID: "addAttachment", Method: "POST", Path: attachmentPath, RequestBody: &huma.RequestBody{Required: true, Content: binaryContent("application/octet-stream")}, BodyReadTimeout: -1, Middlewares: huma.Middlewares{rawBody}}, func(ctx context.Context, in *api.AddAttachmentRequest) (*api.AddAttachmentResponse, error) {
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
		if !metadata.ValidAttachmentName(e.Name) {
			return nil, attachmentError(400, "attachment_invalid")
		}
		if typ := attachmentTypes[strings.ToLower(path.Ext(e.Name))]; typ != "" {
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
		parent, err := cfg.Docbank.EnsureFolder(ctx, path.Join(cfg.DocbankFolder, iss.ProjectUID))
		if err != nil {
			return nil, docbankError(err, docbankUploadErrors)
		}
		sha := hex.EncodeToString(hash.Sum(nil))
		e.DocbankVersion, err = cfg.Docbank.Upload(ctx, parent, e.Name, file, e.Size, sha)
		if err != nil {
			return nil, docbankError(err, docbankUploadErrors)
		}
		id, err := uid.New()
		if err != nil {
			return nil, internalAPIError(err)
		}
		raw, err := json.Marshal(e)
		if err != nil {
			return nil, internalAPIError(err)
		}
		if err := recheckAttachmentWrite(ctx, cfg, in.ProjectID, iss, actor); err != nil {
			return nil, err
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
		return &api.AddAttachmentResponse{Body: api.AddAttachmentResponseBody{ID: id, Attachment: e}}, nil
	})
	huma.Register(h, huma.Operation{OperationID: "attachmentContent", Method: "GET", Path: attachmentPath + "/{attachment_id}/content", Responses: map[string]*huma.Response{"200": {Description: "Attachment content.", Content: binaryContent("*/*")}}}, func(ctx context.Context, in *api.AttachmentReadRequest) (*huma.StreamResponse, error) {
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
			return nil, docbankError(err, docbankContentErrors)
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
