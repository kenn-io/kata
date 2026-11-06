package docbank

import (
	"context"
	"encoding/json/v2"
	"net/url"
	"strconv"
	"uuid"
)

// Node is the file identity returned by Docbank's path and node reads.
type Node struct {
	// Path is supplied by Docbank's live single-node/path reads, never inferred
	// from caller input. Federation uses it to bound the exposed subtree.
	Path             string    `json:"path"`
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Kind             string    `json:"kind"`
	Size             int64     `json:"size"`
	CurrentVersionID uuid.UUID `json:"current_version_id"`
	TrashedAt        string    `json:"trashed_at"`
}

// Version describes immutable content without transferring its bytes.
type Version struct {
	ID     uuid.UUID `json:"id"`
	NodeID int64     `json:"node_id"`
	Size   int64     `json:"size"`
}

// ChildrenPage includes the directory and children from one native snapshot.
type ChildrenPage struct {
	Directory Node   `json:"directory"`
	Items     []Node `json:"items"`
	Total     int    `json:"total"`
	Limit     int    `json:"limit"`
	Offset    int    `json:"offset"`
}

// UnmarshalJSON distinguishes an explicit empty page from an incomplete reply.
func (p *ChildrenPage) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Directory *Node   `json:"directory"`
		Items     *[]Node `json:"items"`
		Total     *int    `json:"total"`
		Limit     *int    `json:"limit"`
		Offset    *int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	if wire.Directory == nil || wire.Items == nil || wire.Total == nil || wire.Limit == nil || wire.Offset == nil {
		return &Error{}
	}
	*p = ChildrenPage{Directory: *wire.Directory, Items: *wire.Items, Total: *wire.Total, Limit: *wire.Limit, Offset: *wire.Offset}
	return nil
}

// Children reads one explicitly bounded page, without creating directories.
func (c *Client) Children(ctx context.Context, id int64, limit, offset int) (ChildrenPage, error) {
	var page ChildrenPage
	query := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	err := c.json(ctx, "GET", "/api/v1/nodes/"+strconv.FormatInt(id, 10)+"/children?"+query.Encode(), nil, &page)
	return page, err
}

// ResolvePath resolves an absolute virtual path.
func (c *Client) ResolvePath(ctx context.Context, path string) (Node, error) {
	var node Node
	err := c.json(ctx, "GET", "/api/v1/path?"+url.Values{"path": {path}}.Encode(), nil, &node)
	return node, err
}

// Node reads a node by numeric identity.
func (c *Client) Node(ctx context.Context, id int64) (Node, error) {
	var node Node
	err := c.json(ctx, "GET", "/api/v1/nodes/"+strconv.FormatInt(id, 10), nil, &node)
	return node, err
}

// Version reads immutable version metadata.
func (c *Client) Version(ctx context.Context, id uuid.UUID) (Version, error) {
	var version Version
	err := c.json(ctx, "GET", "/api/v1/versions/"+id.String(), nil, &version)
	return version, err
}
