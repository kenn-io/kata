//nolint:revive // api is the public package namespace.
package api

type AddAttachmentRequest struct {
	ProjectID int64  `path:"project_id"`
	Ref       string `path:"ref"`
	Name      string `query:"name"`
	Actor     string `query:"actor"`
}
type AttachmentReadRequest struct {
	ProjectID    int64  `path:"project_id"`
	Ref          string `path:"ref"`
	AttachmentID string `path:"attachment_id"`
}
