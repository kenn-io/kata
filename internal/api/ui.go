package api //nolint:revive // package name "api" is the public wire namespace.

import (
	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/kata/internal/db"
)

// UISnapshotContractVersion identifies the browser snapshot wire contract.
const UISnapshotContractVersion = "1"

// UICapabilities describes the browser behavior authorized for this request.
type UICapabilities struct {
	Writable    bool   `json:"writable"`
	Updates     string `json:"updates" enum:"sse,poll"`
	ActorPolicy string `json:"actor_policy"`
}

// UISnapshotRequest is the normalized route and projection intent accepted by
// GET /api/v1/ui/snapshot.
type UISnapshotRequest struct {
	IfNoneMatch      string      `header:"If-None-Match"`
	View             string      `query:"view"`
	ProjectUID       string      `query:"project_uid"`
	Status           []string    `query:"status"`
	Owner            []string    `query:"owner"`
	Label            []string    `query:"label"`
	Relationship     []string    `query:"relationship" enum:"parent,child,blocks,blocked_by,related"`
	Text             string      `query:"text"`
	SelectedIssueUID string      `query:"selected_issue_uid"`
	IncludeGraph     bool        `query:"include_graph"`
	IncludeHistory   bool        `query:"include_history"`
	LocalDate        string      `query:"local_date"`
	TimeZone         string      `query:"time_zone"`
	Limit            OptionalInt `query:"limit"`
}

// Resolve preserves every repeated multivalue filter. Huma's scalar-oriented
// query binding supplies the first value for slices on this adapter, while the
// browser contract uses exploded repeated parameters.
func (in *UISnapshotRequest) Resolve(ctx huma.Context) []error {
	requestURL := ctx.URL()
	query := requestURL.Query()
	in.Status = append([]string(nil), query["status"]...)
	in.Owner = append([]string(nil), query["owner"]...)
	in.Label = append([]string(nil), query["label"]...)
	in.Relationship = append([]string(nil), query["relationship"]...)
	return nil
}

// UISelectedAuthority is the selected issue and its authoritative detail.
type UISelectedAuthority struct {
	State       string          `json:"state"`
	Issue       *db.UIIssue     `json:"issue,omitempty"`
	Comments    []db.Comment    `json:"comments"`
	Labels      []db.IssueLabel `json:"labels"`
	Links       []db.UILink     `json:"links"`
	Recurrences []db.Recurrence `json:"recurrences"`
	History     []db.Event      `json:"history"`
}

// UIGraph is the optional relationship projection for the selected intent.
type UIGraph struct {
	Issues         []db.UIIssue              `json:"issues"`
	Links          []db.UILink               `json:"links"`
	Edges          []db.UIGraphEdge          `json:"edges"`
	UnresolvedRefs []db.UIGraphUnresolvedRef `json:"unresolved_refs"`
}

// UISnapshotResponseBody is the coherent browser read envelope.
type UISnapshotResponseBody struct {
	ContractVersion string               `json:"contract_version"`
	Cursor          int64                `json:"cursor"`
	Capabilities    UICapabilities       `json:"capabilities"`
	Origin          string               `json:"origin"`
	OriginStable    bool                 `json:"origin_stable"`
	Catalog         []db.UIProject       `json:"catalog"`
	Collection      []db.UIIssue         `json:"collection"`
	CollectionLinks []db.UILink          `json:"collection_links"`
	Selected        *UISelectedAuthority `json:"selected,omitempty"`
	Graph           *UIGraph             `json:"graph,omitempty"`
}

// UISnapshotResponse supports either a coherent 200 body or an empty 304.
type UISnapshotResponse struct {
	Status int
	ETag   string `header:"ETag"`
	Body   UISnapshotResponseBody
}

// UIReferencesRequest bounds browser typeahead and catalog choices.
type UIReferencesRequest struct {
	IfNoneMatch string      `header:"If-None-Match"`
	Query       string      `query:"q"`
	ProjectUID  string      `query:"project_uid"`
	Limit       OptionalInt `query:"limit"`
}

// UIReferencesResponseBody contains bounded, coherent reference choices.
type UIReferencesResponseBody struct {
	ContractVersion string                `json:"contract_version"`
	Cursor          int64                 `json:"cursor"`
	Capabilities    UICapabilities        `json:"capabilities"`
	Origin          string                `json:"origin"`
	OriginStable    bool                  `json:"origin_stable"`
	Projects        []db.Project          `json:"projects"`
	Issues          []db.UIIssueReference `json:"issues"`
	Owners          []string              `json:"owners"`
	Labels          []string              `json:"labels"`
}

// UIReferencesResponse is the bounded reference response and validator.
type UIReferencesResponse struct {
	Status int
	ETag   string `header:"ETag"`
	Body   UIReferencesResponseBody
}
