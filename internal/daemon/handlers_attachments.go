package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
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
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/config"
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
	if _, ok := errors.AsType[*docbank.KeyError](err); ok {
		return docbankKeyUnavailable(err)
	}
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

// docbankKeyUnavailable gives clients the reason but not the key source, which
// names a daemon-host path or environment variable; health and the daemon log carry it.
func docbankKeyUnavailable(err error) error {
	message := "Docbank API key unavailable"
	if e, ok := errors.AsType[*config.CredentialSourceError](err); ok {
		message += ": " + e.Reason
	}
	return api.NewError(http.StatusServiceUnavailable, "docbank_key_unavailable", message, "Run kata doctor on the daemon host to see which [docbank] key source failed.", nil)
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
	huma.Register(h, huma.Operation{OperationID: "linkAttachment", Method: "POST", Path: attachmentPath + "/links"}, func(ctx context.Context, in *api.LinkAttachmentRequest) (*api.AddAttachmentResponse, error) {
		return linkAttachment(ctx, cfg, in)
	})
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
		e := metadata.Attachment{Name: in.Name}
		if !metadata.ValidAttachmentName(e.Name) {
			return nil, attachmentError(400, "attachment_invalid")
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
		return saveAttachment(ctx, cfg, in.ProjectID, iss, actor, e, nil)
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

func saveAttachment(ctx context.Context, cfg ServerConfig, projectID int64, iss db.Issue, actor string, e metadata.Attachment, reuseVersion *uuid.UUID) (*api.AddAttachmentResponse, error) {
	e.MediaType = "application/octet-stream"
	if typ := attachmentTypes[strings.ToLower(path.Ext(e.Name))]; typ != "" {
		e.MediaType = typ
	}
	id, err := uid.New()
	if err != nil {
		return nil, internalAPIError(err)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, internalAPIError(err)
	}
	if err := recheckAttachmentWrite(ctx, cfg, projectID, iss, actor); err != nil {
		return nil, err
	}
	patch := map[string]jsontext.Value{metadata.AttachmentPrefix + id: raw}
	if reuseVersion != nil {
		// Recheck under the metadata write transaction so concurrent retries
		// cannot each observe an absent reference and create a second one.
		ctx = db.WithMetadataPatchHook(ctx, func(_ context.Context, _ *sql.Tx, current db.Issue) (map[string]jsontext.Value, error) {
			existingID, _, err := attachmentForVersion(current, docbank.Version{ID: *reuseVersion})
			if err != nil {
				return nil, err
			}
			if existingID != "" {
				return map[string]jsontext.Value{}, nil
			}
			return patch, nil
		})
	}
	res, err := cfg.DB.PatchIssueMetadata(ctx, db.PatchIssueMetadataIn{IssueID: iss.ID, Actor: actor, Patch: patch})
	if errors.Is(err, db.ErrFederatedReadOnly) {
		return nil, federationReadOnlyError(err)
	}
	if err != nil {
		return nil, internalAPIError(err)
	}
	if res.Changed {
		cfg.Publish().Event(res.Event.ProjectID, res.Event)
	}
	if reuseVersion != nil {
		id, e, err = attachmentForVersion(res.Issue, docbank.Version{ID: *reuseVersion})
		if err != nil {
			return nil, internalAPIError(err)
		}
		if id == "" {
			return nil, internalAPIError(errors.New("reused attachment missing after metadata patch"))
		}
	}
	return &api.AddAttachmentResponse{Body: api.AddAttachmentResponseBody{ID: id, Attachment: e}}, nil
}
