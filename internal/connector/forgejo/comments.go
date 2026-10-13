package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *user     `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Page numbers are constructed locally; provider pagination links never choose
// the next request's origin or path. Continue until empty even with a lower
// provider-side page cap than the requested limit.
func pages[T any](ctx context.Context, c *client, path string) ([]T, *connector.Error) {
	all := []T{}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	for page := 1; page <= 10000; page++ {
		var batch []T
		if err := c.request(ctx, "GET", fmt.Sprintf("%s%spage=%d&limit=50", path, separator, page), nil, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return all, nil
		}
		all = append(all, batch...)
	}
	return nil, failure("provider_error", "Forgejo pagination exceeded its limit")
}
func (c *client) scope(root string) string { return digest([]string{c.instance, root}) }
func (c *client) marker(root string, actor int64, operation string) string {
	hash := sha256.Sum256([]byte(operation))
	return "<!-- kata-publish:" + c.scope(root) + ":" + id(actor) + ":" + hex.EncodeToString(hash[:]) + " -->"
}
func (c *client) hasMarker(body, root, operation string) bool {
	hash := sha256.Sum256([]byte(operation))
	operationHash := hex.EncodeToString(hash[:])
	prefix := "<!-- kata-publish:" + c.scope(root) + ":"
	for len(body) > 0 {
		index := strings.Index(body, prefix)
		if index < 0 {
			return false
		}
		body = body[index+len(prefix):]
		end := strings.Index(body, " -->")
		next := strings.Index(body, prefix)
		if next >= 0 && (end < 0 || next < end) {
			body = body[next:]
			continue
		}
		if end < 0 {
			return false
		}
		marker := body[:end]
		if marker == operationHash {
			return true
		}
		actor, candidateHash, actorScoped := strings.Cut(marker, ":")
		if actorScoped && candidateHash == operationHash {
			if _, ok := numeric(actor); ok {
				return true
			}
		}
		body = body[end+len(" -->"):]
	}
	return false
}
func (c *client) visibleBody(raw comment, root string, self int64) string {
	if raw.User == nil {
		return raw.Body
	}
	prefix := "\n\n<!-- kata-publish:" + c.scope(root) + ":"
	index := strings.LastIndex(raw.Body, prefix)
	if index < 0 {
		return raw.Body
	}
	suffix := raw.Body[index+len(prefix):]
	marker, trailing, closed := strings.Cut(suffix, " -->")
	if !closed {
		return raw.Body
	}
	actor, hash, actorScoped := strings.Cut(marker, ":")
	if actorScoped {
		if raw.User.ID <= 0 || actor != id(raw.User.ID) {
			return raw.Body
		}
	} else {
		// Older markers did not record their publishing actor, so only the
		// current account can safely claim one without hiding a quoted marker.
		if raw.User.ID != self || len(marker) != 64 {
			return raw.Body
		}
		hash = marker
	}
	if len(hash) != 64 {
		return raw.Body
	}
	if _, e := hex.DecodeString(hash); e != nil || strings.ToLower(hash) != hash {
		return raw.Body
	}
	return raw.Body[:index] + trailing
}
func (c *client) normalizeComment(raw comment, root string, self int64) (connector.Comment, *connector.Error) {
	if raw.ID <= 0 || (raw.User != nil && raw.User.ID == 0) {
		return connector.Comment{}, failure("provider_error", "Forgejo comment identity is invalid")
	}
	author := connector.Actor{ID: "ghost", DisplayName: "Ghost"}
	if raw.User != nil {
		author = connector.Actor{ID: id(raw.User.ID), DisplayName: raw.User.Login}
	}
	v := connector.Comment{ID: id(raw.ID), Body: c.visibleBody(raw, root, self), Author: author, CreatedAt: raw.CreatedAt, UpdatedAt: raw.UpdatedAt}
	v.Revision = digest(v)
	return v, nil
}
func (h *handler) ListComments(ctx context.Context, p connector.ListCommentsParams) (connector.ListCommentsResult, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	_, path, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	self, err := c.self(ctx)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	raw, err := pages[comment](ctx, c, path+"/comments")
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	sort.Slice(raw, func(a, b int) bool {
		if raw[a].CreatedAt.Equal(raw[b].CreatedAt) {
			return id(raw[a].ID) < id(raw[b].ID)
		}
		return raw[a].CreatedAt.Before(raw[b].CreatedAt)
	})
	out := connector.ListCommentsResult{Comments: []connector.Comment{}}
	seen := map[int64]bool{}
	for _, item := range raw {
		if seen[item.ID] {
			return connector.ListCommentsResult{}, failure("provider_error", "Forgejo returned duplicate comments")
		}
		seen[item.ID] = true
		v, err := c.normalizeComment(item, p.RootKey, self.ID)
		if err != nil {
			return connector.ListCommentsResult{}, err
		}
		out.Comments = append(out.Comments, v)
	}
	return out, nil
}
func (h *handler) PublishComment(ctx context.Context, p connector.PublishCommentParams) (connector.Comment, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Comment{}, err
	}
	_, path, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.Comment{}, err
	}
	self, err := c.self(ctx)
	if err != nil {
		return connector.Comment{}, err
	}
	if !connector.ValidOperationID(p.OperationID) {
		return connector.Comment{}, failure("invalid_request", "Forgejo publication operation is invalid")
	}
	marker := c.marker(p.RootKey, self.ID, p.OperationID)
	body := p.Body + "\n\n" + marker
	raw, err := pages[comment](ctx, c, path+"/comments")
	if err != nil {
		return connector.Comment{}, err
	}
	var adopted *comment
	foreignMarker := false
	for index := range raw {
		item := &raw[index]
		if !c.hasMarker(item.Body, p.RootKey, p.OperationID) {
			continue
		}
		// A foreign marker can be quoted prose or an edited prior delivery.
		// Defer that ambiguity while looking for a unique valid self delivery;
		// without one, the marker still forbids creating a duplicate.
		if item.User != nil && item.User.ID != self.ID && item.Body != body {
			foreignMarker = true
			continue
		}
		if item.Body != body || item.User == nil || item.User.ID != self.ID || adopted != nil {
			return connector.Comment{}, failure("publication_conflict", "Forgejo publication marker is conflicting or ambiguous")
		}
		adopted = item
	}
	if adopted != nil {
		return c.normalizeComment(*adopted, p.RootKey, self.ID)
	}
	if foreignMarker {
		return connector.Comment{}, failure("publication_conflict", "Forgejo publication marker is conflicting or ambiguous")
	}
	var created comment
	if err := c.request(ctx, "POST", path+"/comments", map[string]string{"body": body}, &created); err != nil {
		return connector.Comment{}, err
	}
	if created.Body != body || created.User == nil || created.User.ID != self.ID {
		return connector.Comment{}, failure("provider_error", "Forgejo publication readback did not match")
	}
	return c.normalizeComment(created, p.RootKey, self.ID)
}
