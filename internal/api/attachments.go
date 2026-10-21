//nolint:revive // api is the public package namespace.
package api

import (
	"mime/multipart"

	"go.kenn.io/kata/internal/metadata"
)

type AddAttachmentRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Actor     string `query:"actor"`
	RawBody   multipart.Form
}
type AttachmentResponse struct {
	Body struct {
		AttachmentID string              `json:"attachment_id"`
		Attachment   metadata.Attachment `json:"attachment"`
	}
}
type RemoveAttachmentRequest struct {
	ProjectID    int64  `path:"project_id"`
	Ref          string `path:"ref"`
	AttachmentID string `path:"attachment_id"`
	Actor        string `query:"actor"`
}
type RemoveAttachmentResponse struct {
	Body struct {
		Changed bool `json:"changed"`
	}
}

type AttachmentReadRequest struct {
	ProjectID    int64  `path:"project_id"`
	Ref          string `path:"ref"`
	AttachmentID string `path:"attachment_id"`
}
