package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

func resolveAttachment(ctx context.Context, cfg ServerConfig, in api.AttachmentReadRequest) (metadata.Attachment, error) {
	iss, err := activeIssueByRef(ctx, cfg.DB, in.ProjectID, in.Ref, db.IncludeDeletedNo)
	if err != nil {
		return metadata.Attachment{}, err
	}
	var values map[string]jsontext.Value
	if err = json.Unmarshal([]byte(iss.Metadata), &values); err != nil {
		return metadata.Attachment{}, internalAPIError(err)
	}
	m, err := metadata.DecodeAttachments(values["attachments"])
	if err != nil {
		return metadata.Attachment{}, attachmentError(400, "attachment_invalid")
	}
	if _, ok := m[in.AttachmentID]; !ok {
		return metadata.Attachment{}, attachmentError(404, "attachment_not_found")
	}
	e, err := m.Lookup(in.AttachmentID)
	if err != nil {
		return e, attachmentError(400, "attachment_invalid")
	}
	if cfg.Docbank == nil {
		return e, attachmentError(503, "docbank_not_configured")
	}
	return e, nil
}
func registerAttachmentReadHandlers(h huma.API, cfg ServerConfig) {
	huma.Register(h, huma.Operation{OperationID: "attachmentContent", Method: "GET", Path: attachmentPath + "/{attachment_id}/content", Responses: map[string]*huma.Response{"200": {Description: "Verified attachment bytes", Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}}}, func(ctx context.Context, in *api.AttachmentReadRequest) (*huma.StreamResponse, error) {
		e, err := resolveAttachment(ctx, cfg, *in)
		if err != nil {
			return nil, err
		}
		stream, err := cfg.Docbank.Content(ctx, e)
		if err != nil {
			return nil, docbankError(err)
		}
		// Read the sniff prefix before committing response headers. Small-file failures
		// remain typed API errors; large-file failures abort before the final chunk.
		head := make([]byte, 512)
		n, err := io.ReadFull(stream, head)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			_ = stream.Close()
			return nil, docbankError(err)
		}
		head = head[:n]
		typ := http.DetectContentType(head)
		inline := typ == "image/png" || typ == "image/jpeg" || typ == "image/gif" || typ == "image/webp" || typ == "video/mp4" || typ == "video/webm"
		if typ == "application/ogg" && e.MediaType == "video/ogg" {
			typ = "video/ogg"
			inline = true
		}
		disposition := "inline"
		if !inline {
			typ = "application/octet-stream"
			disposition = "attachment"
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = stream.Close() }()
			hctx.SetHeader("Content-Type", typ)
			hctx.SetHeader("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": cleanAttachmentName(e.Name)}))
			hctx.SetHeader("Content-Length", strconv.FormatInt(e.Size, 10))
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetHeader("Content-Security-Policy", "sandbox")
			if err := copyVerifiedAttachment(hctx.BodyWriter(), io.MultiReader(bytes.NewReader(head), stream)); err != nil {
				panic(http.ErrAbortHandler)
			}
		}}, nil
	})
}

// Keep one chunk pending until EOF validation so Content-Length cannot make an
// integrity failure look like a complete download to the receiving client.
func copyVerifiedAttachment(w io.Writer, r io.Reader) error {
	pending := make([]byte, 32<<10)
	next := make([]byte, len(pending))
	n, err := io.ReadFull(r, pending)
	for {
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		if err != nil {
			_, err = w.Write(pending[:n])
			return err
		}
		m, readErr := io.ReadFull(r, next)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if _, err = w.Write(pending[:n]); err != nil {
			return err
		}
		pending, next = next, pending
		n, err = m, readErr
	}
}
