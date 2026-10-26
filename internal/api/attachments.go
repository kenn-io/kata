//nolint:revive // api is the public package namespace.
package api

import "go.kenn.io/kata/internal/metadata"

type AddAttachmentRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Name      string `query:"name" required:"true"`
	Actor     string `query:"actor"`
}
type AttachmentReadRequest struct {
	ProjectID    int64  `path:"project_id"`
	Ref          string `path:"ref"`
	AttachmentID string `path:"attachment_id"`
}

// AddAttachmentResponseBody is the reference saved for one upload.
type AddAttachmentResponseBody struct {
	ID         string              `json:"attachment_id"`
	Attachment metadata.Attachment `json:"attachment"`
}

type AddAttachmentResponse struct {
	Body AddAttachmentResponseBody
}
