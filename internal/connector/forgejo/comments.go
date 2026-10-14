package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/kata/pkg/connector"
)

// pages requests numbered pages until one is empty. Page numbers are
// constructed locally; provider pagination links never choose the next
// request's origin or path. Continue until empty even with a lower
// provider-side page cap than the requested limit.
func pages[T any](fetch func(forgejosdk.ListOptions) ([]T, *forgejosdk.Response, error)) ([]T, *connector.Error) {
	all := []T{}
	for page := 1; page <= 10000; page++ {
		batch, response, err := fetch(forgejosdk.ListOptions{Page: page, PageSize: 50})
		if err != nil {
			return nil, providerFailure(response)
		}
		if len(batch) == 0 {
			return all, nil
		}
		all = append(all, batch...)
	}
	return nil, failure("provider_error", "Forgejo pagination exceeded its limit")
}

// comments lists every comment on an issue. Forgejo does not paginate this
// endpoint, so one request returns them all.
func (c *client) comments(ref issueRef) ([]*forgejosdk.Comment, *connector.Error) {
	comments, response, err := c.api.ListIssueComments(ref.owner, ref.repo, ref.number, forgejosdk.ListIssueCommentOptions{})
	if err != nil {
		return nil, providerFailure(response)
	}
	return comments, nil
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
func (c *client) visibleBody(raw *forgejosdk.Comment, root string, self int64) string {
	if raw.Poster == nil {
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
		if raw.Poster.ID <= 0 || actor != id(raw.Poster.ID) {
			return raw.Body
		}
	} else {
		// Older markers did not record their publishing actor, so only the
		// current account can safely claim one without hiding a quoted marker.
		if raw.Poster.ID != self || len(marker) != 64 {
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
func (c *client) normalizeComment(raw *forgejosdk.Comment, root string, self int64) (connector.Comment, *connector.Error) {
	if raw.ID <= 0 || (raw.Poster != nil && raw.Poster.ID == 0) {
		return connector.Comment{}, failure("provider_error", "Forgejo comment identity is invalid")
	}
	author := connector.Actor{ID: "ghost", DisplayName: "Ghost"}
	if raw.Poster != nil {
		author = connector.Actor{ID: id(raw.Poster.ID), DisplayName: raw.Poster.UserName}
	}
	v := connector.Comment{ID: id(raw.ID), Body: c.visibleBody(raw, root, self), Author: author, CreatedAt: raw.Created, UpdatedAt: raw.Updated}
	v.Revision = digest(v)
	return v, nil
}
func (h *handler) ListComments(ctx context.Context, p connector.ListCommentsParams) (connector.ListCommentsResult, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	_, ref, err := c.load(p.RootKey)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	self, err := c.self()
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	raw, err := c.comments(ref)
	if err != nil {
		return connector.ListCommentsResult{}, err
	}
	for _, item := range raw {
		if item == nil {
			return connector.ListCommentsResult{}, failure("provider_error", "Forgejo comment identity is invalid")
		}
	}
	sort.Slice(raw, func(a, b int) bool {
		if raw[a].Created.Equal(raw[b].Created) {
			return id(raw[a].ID) < id(raw[b].ID)
		}
		return raw[a].Created.Before(raw[b].Created)
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
	_, ref, err := c.load(p.RootKey)
	if err != nil {
		return connector.Comment{}, err
	}
	self, err := c.self()
	if err != nil {
		return connector.Comment{}, err
	}
	if !connector.ValidOperationID(p.OperationID) {
		return connector.Comment{}, failure("invalid_request", "Forgejo publication operation is invalid")
	}
	marker := c.marker(p.RootKey, self.ID, p.OperationID)
	body := p.Body + "\n\n" + marker
	raw, err := c.comments(ref)
	if err != nil {
		return connector.Comment{}, err
	}
	var adopted *forgejosdk.Comment
	foreignMarker := false
	for _, item := range raw {
		if item == nil || !c.hasMarker(item.Body, p.RootKey, p.OperationID) {
			continue
		}
		// A foreign marker can be quoted prose or an edited prior delivery.
		// Defer that ambiguity while looking for a unique valid self delivery;
		// without one, the marker still forbids creating a duplicate.
		if item.Poster != nil && item.Poster.ID != self.ID && item.Body != body {
			foreignMarker = true
			continue
		}
		if item.Body != body || item.Poster == nil || item.Poster.ID != self.ID || adopted != nil {
			return connector.Comment{}, failure("publication_conflict", "Forgejo publication marker is conflicting or ambiguous")
		}
		adopted = item
	}
	if adopted != nil {
		return c.normalizeComment(adopted, p.RootKey, self.ID)
	}
	if foreignMarker {
		return connector.Comment{}, failure("publication_conflict", "Forgejo publication marker is conflicting or ambiguous")
	}
	created, response, e := c.api.CreateIssueComment(ref.owner, ref.repo, ref.number, forgejosdk.CreateIssueCommentOption{Body: body})
	if e != nil {
		return connector.Comment{}, providerFailure(response)
	}
	if created.Body != body || created.Poster == nil || created.Poster.ID != self.ID {
		return connector.Comment{}, failure("provider_error", "Forgejo publication readback did not match")
	}
	return c.normalizeComment(created, p.RootKey, self.ID)
}
