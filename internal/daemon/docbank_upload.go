package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func attachmentRawBody(ctx huma.Context, next func(huma.Context)) {
	r, w := humago.Unwrap(ctx)
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	next(huma.WithContext(ctx, context.WithValue(ctx.Context(), attachmentUploadContextKey{}, http.MaxBytesReader(w, r.Body, maxAttachmentBytes))))
}

func uploadDocbankAttachment(ctx context.Context, cfg ServerConfig, iss db.Issue, name string, reader io.Reader, recheck func() error) (metadata.Attachment, error) {
	if cfg.Docbank == nil {
		return metadata.Attachment{}, attachmentError(503, "docbank_not_configured")
	}
	e := metadata.Attachment{Name: name}
	if !metadata.ValidAttachmentName(e.Name) {
		return metadata.Attachment{}, attachmentError(400, "attachment_invalid")
	}
	file, err := os.CreateTemp("", "kata-attachment-*")
	if err != nil {
		return metadata.Attachment{}, internalAPIError(err)
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	hash := sha256.New()
	e.Size, err = io.Copy(io.MultiWriter(file, hash), reader)
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return metadata.Attachment{}, attachmentError(413, "attachment_too_large")
	}
	if err != nil {
		return metadata.Attachment{}, internalAPIError(err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return metadata.Attachment{}, internalAPIError(err)
	}
	if recheck != nil {
		if err := recheck(); err != nil {
			return metadata.Attachment{}, err
		}
	}
	parent, err := cfg.Docbank.EnsureFolder(ctx, path.Join(cfg.DocbankFolder, iss.ProjectUID))
	if err != nil {
		return metadata.Attachment{}, docbankError(err, docbankUploadErrors)
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	e.DocbankVersion, err = cfg.Docbank.Upload(ctx, parent, e.Name, file, e.Size, sha)
	if err != nil {
		return metadata.Attachment{}, docbankError(err, docbankUploadErrors)
	}
	e.MediaType = attachmentMediaType(e.Name)
	return e, nil
}
