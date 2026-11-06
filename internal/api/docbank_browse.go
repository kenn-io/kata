//nolint:revive // api is the public package namespace.
package api

import "uuid"

// DocbankBrowseNode is an explicit catalog projection, without source metadata.
type DocbankBrowseNode struct {
	ID               int64      `json:"id" minimum:"1"`
	Name             string     `json:"name"`
	Kind             string     `json:"kind" enum:"dir,file"`
	Size             int64      `json:"size" minimum:"0"`
	Path             string     `json:"path" maxLength:"4096"`
	CurrentVersionID *uuid.UUID `json:"current_version_id,omitempty"`
}

type DocbankBrowseParams struct {
	Path   string `query:"path" maxLength:"4096"`
	Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"200"`
	Offset int    `query:"offset" default:"0" minimum:"0"`
}

type DocbankChildrenRequest struct {
	ProjectID int64 `path:"project_id"`
	DocbankBrowseParams
}

type DocbankResolveRequest struct {
	ProjectID int64  `path:"project_id"`
	Path      string `query:"path" required:"true" maxLength:"4096"`
}

type DocbankServiceChildrenRequest struct {
	DocbankServiceRequest
	DocbankBrowseParams
	Origin string `query:"origin" required:"true"`
}

type DocbankServiceResolveRequest struct {
	DocbankServiceRequest
	Origin string `query:"origin" required:"true"`
	Path   string `query:"path" required:"true" maxLength:"4096"`
}

type DocbankResolveBody struct {
	ProjectUID string            `json:"project_uid"`
	Origin     string            `json:"origin"`
	Node       DocbankBrowseNode `json:"node"`
}

type DocbankChildrenBody struct {
	ProjectUID string              `json:"project_uid"`
	Origin     string              `json:"origin"`
	Directory  DocbankBrowseNode   `json:"directory"`
	Items      []DocbankBrowseNode `json:"items"`
	Total      int                 `json:"total" minimum:"0"`
	Limit      int                 `json:"limit" minimum:"1" maximum:"200"`
	Offset     int                 `json:"offset" minimum:"0"`
}

type DocbankResolveResponse struct{ Body DocbankResolveBody }
type DocbankChildrenResponse struct{ Body DocbankChildrenBody }
