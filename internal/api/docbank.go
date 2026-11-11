//nolint:revive // api is the public package namespace.
package api

import (
	"uuid"

	"go.kenn.io/kata/internal/metadata"
)

// DocbankServiceRequest binds a transport request to a native project identity.
type DocbankServiceRequest struct {
	ProjectID     int64  `path:"project_id"`
	ProjectUID    string `query:"project_uid" required:"true"`
	Authorization string `header:"Authorization"`
}

// DocbankServiceCapabilities is computed through the complete upstream path.
type DocbankServiceCapabilities struct {
	Version    int    `json:"version"`
	ProjectUID string `json:"project_uid"`
	Origin     string `json:"origin"`
}

type DocbankServiceCapabilitiesResponse struct{ Body DocbankServiceCapabilities }

// DocbankDescriptor contains no attachment ID: only the initiating daemon
// creates the metadata reference through its ordinary local-first event flow.
type DocbankDescriptor struct {
	ProjectUID string              `json:"project_uid"`
	Attachment metadata.Attachment `json:"attachment"`
}

type DocbankDescriptorResponse struct{ Body DocbankDescriptor }

type DocbankUploadRequest struct {
	Origin string `query:"origin" required:"true"`
	DocbankServiceRequest
	Ref        string `path:"ref"`
	Name       string `query:"name" required:"true"`
	ClientKind string `query:"client_kind"`
}

type DocbankLinkRequest struct {
	Origin string `query:"origin" required:"true"`
	DocbankServiceRequest
	Ref        string `path:"ref"`
	ClientKind string `query:"client_kind"`
	Body       LinkAttachmentInput
}

type DocbankContentRequest struct {
	DocbankServiceRequest
	VersionID uuid.UUID `path:"version_id"`
	Origin    string    `query:"origin" required:"true"`
}
