package docbank

import (
	"context"
	"net/url"
	"strconv"
	"uuid"
)

// Node is the file identity returned by Docbank's path and node reads.
type Node struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Kind             string    `json:"kind"`
	CurrentVersionID uuid.UUID `json:"current_version_id"`
	TrashedAt        string    `json:"trashed_at"`
}

// Version describes immutable content without transferring its bytes.
type Version struct {
	ID     uuid.UUID `json:"id"`
	NodeID int64     `json:"node_id"`
	Size   int64     `json:"size"`
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
