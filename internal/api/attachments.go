//nolint:revive // api is the public package namespace.
package api

import (
	"uuid"

	"go.kenn.io/kata/internal/metadata"
)

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

// LinkAttachmentInput selects an existing Docbank file version.
type LinkAttachmentInput struct {
	NodeID    *int64     `json:"node_id,omitempty"`
	Path      *string    `json:"path,omitempty"`
	VersionID *uuid.UUID `json:"version_id,omitempty"`
	Actor     string     `json:"actor,omitempty"`
}

type LinkAttachmentRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Body      LinkAttachmentInput
}
