package docbank

import (
	"context"
	"net/url"
	"strconv"
	"uuid"
)

// Node is the file identity returned by Docbank's path and node reads.
// A node may be a directory or be trashed; callers choose whether to accept it.
type Node struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Kind             string    `json:"kind"`
	CurrentVersionID uuid.UUID `json:"current_version_id"`
	TrashedAt        string    `json:"trashed_at"`
}

// Version describes immutable content without transferring its bytes.
type Version struct {
	ID        uuid.UUID `json:"id"`
	NodeID    int64     `json:"node_id"`
	Size      int64     `json:"size"`
	MediaType string    `json:"mime_type"`
}

// ResolvePath resolves an absolute virtual path through the configured origin.
func (c *Client) ResolvePath(ctx context.Context, path string) (Node, error) {
	return c.readNode(ctx, "/api/v1/path?"+url.Values{"path": {path}}.Encode(), 0)
}

// Node reads a live or trashed node by its positive numeric identity.
func (c *Client) Node(ctx context.Context, id int64) (Node, error) {
	return c.readNode(ctx, "/api/v1/nodes/"+strconv.FormatInt(id, 10), id)
}

func (c *Client) readNode(ctx context.Context, endpoint string, id int64) (Node, error) {
	var node Node
	if err := c.json(ctx, "GET", endpoint, nil, &node); err != nil {
		return Node{}, err
	}
	if node.ID <= 0 || (id != 0 && node.ID != id) || (node.Kind == "file" && node.CurrentVersionID == uuid.Nil()) {
		return Node{}, &Error{}
	}
	return node, nil
}

// Version reads immutable metadata and rejects incomplete or mismatched identity.
func (c *Client) Version(ctx context.Context, id uuid.UUID) (Version, error) {
	var wire struct {
		Version
		Size *int64 `json:"size"`
	}
	if err := c.json(ctx, "GET", "/api/v1/versions/"+id.String(), nil, &wire); err != nil {
		return Version{}, err
	}
	if wire.ID != id || wire.NodeID <= 0 || wire.Size == nil || *wire.Size < 0 {
		return Version{}, &Error{}
	}
	wire.Version.Size = *wire.Size
	return wire.Version, nil
}
