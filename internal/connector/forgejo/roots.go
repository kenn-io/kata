package forgejo

import (
	"context"
	"net/url"
	"strings"
	"time"

	forgejosdk "codeberg.org/mvdkleijn/forgejo-sdk/forgejo/v3"
	"go.kenn.io/kata/pkg/connector"
)

// issueRef names an issue by the repository path the provider currently reports.
type issueRef struct {
	owner, repo string
	number      int64
}
type rootKey struct{ repo, number, issue int64 }

func parseKey(s string) (rootKey, bool) {
	repo, rest, ok := strings.Cut(s, "#")
	number, issue, ok2 := strings.Cut(rest, "@")
	r, a := numeric(repo)
	n, b := numeric(number)
	i, c := numeric(issue)
	return rootKey{r, n, i}, ok && ok2 && a && b && c
}
func splitRepoName(name string) (owner, repo string, ok bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return "", "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part {
			return "", "", false
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
				return "", "", false
			}
		}
	}
	return parts[0], parts[1], true
}
func (c *client) locator(s string) (issueRef, *connector.Error) {
	var name, number string
	if strings.Contains(s, "://") {
		u, e := url.Parse(s)
		if e != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(s, "#") || u.RawPath != "" || normalizedOrigin(u) != c.origin {
			return issueRef{}, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
		p := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(p) != 4 || p[2] != "issues" {
			return issueRef{}, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
		name = p[0] + "/" + p[1]
		number = p[3]
	} else {
		var ok bool
		name, number, ok = strings.Cut(s, "#")
		if !ok {
			return issueRef{}, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
	}
	owner, repo, ok := splitRepoName(name)
	n, valid := numeric(number)
	if !ok || !valid {
		return issueRef{}, failure("invalid_locator", "Forgejo issue locator is invalid")
	}
	return issueRef{owner: owner, repo: repo, number: n}, nil
}
func (c *client) load(key string) (*forgejosdk.Issue, issueRef, *connector.Error) {
	k, ok := parseKey(key)
	if !ok {
		return nil, issueRef{}, failure("invalid_root", "Forgejo root key is invalid")
	}
	repo, response, e := c.api.GetRepoByID(k.repo)
	if e != nil {
		return nil, issueRef{}, rootFailure(response)
	}
	owner, name, ok := splitRepoName(repo.FullName)
	if !ok || repo.ID != k.repo {
		return nil, issueRef{}, failure("identity_mismatch", "Forgejo repository identity changed")
	}
	return c.loadIssue(issueRef{owner: owner, repo: name, number: k.number}, k)
}
func rootFailure(response *forgejosdk.Response) *connector.Error {
	err := providerFailure(response)
	if err.Code == "not_found" {
		err.Code = "root_not_found"
	}
	return err
}
func (c *client) loadIssue(ref issueRef, k rootKey) (*forgejosdk.Issue, issueRef, *connector.Error) {
	i, response, e := c.api.GetIssue(ref.owner, ref.repo, ref.number)
	if e != nil {
		return nil, ref, rootFailure(response)
	}
	if i.ID <= 0 || i.Index != k.number || (k.issue != 0 && i.ID != k.issue) || i.Repository == nil || i.Repository.ID != k.repo {
		return i, ref, failure("identity_mismatch", "Forgejo issue identity changed")
	}
	if i.PullRequest != nil {
		return i, ref, failure("unsupported", "Forgejo pull requests are unsupported")
	}
	if i.State != forgejosdk.StateOpen && i.State != forgejosdk.StateClosed {
		return i, ref, failure("provider_error", "Forgejo issue state is invalid")
	}
	if _, err := fieldValues(i); err != nil {
		return i, ref, err
	}
	return i, ref, nil
}
func (c *client) root(i *forgejosdk.Issue) connector.Root {
	state := "open"
	if i.State == forgejosdk.StateClosed {
		state = "complete"
	}
	root := connector.Root{Key: id(i.Repository.ID) + "#" + id(i.Index) + "@" + id(i.ID), IdentityKey: c.origin, Title: i.Title, Body: i.Body, State: state, UpdatedAt: i.Updated}
	root.Fields, _ = fieldValues(i)
	root.Revision = digest(root)
	// Forgejo stamps updated_at with its own clock, which can run ahead of
	// ours. An observation cannot predate the update it observed.
	root.ObservedAt = time.Now().UTC()
	if root.ObservedAt.Before(i.Updated) {
		root.ObservedAt = i.Updated
	}
	return root
}
func (h *handler) ResolveRoot(ctx context.Context, p connector.ResolveRootParams) (connector.Root, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Root{}, err
	}
	ref, err := c.locator(p.Locator)
	if err != nil {
		return connector.Root{}, err
	}
	repo, response, e := c.api.GetRepo(ref.owner, ref.repo)
	if e != nil {
		return connector.Root{}, rootFailure(response)
	}
	owner, name, ok := splitRepoName(repo.FullName)
	if !ok || repo.ID <= 0 {
		return connector.Root{}, failure("provider_error", "Forgejo repository is invalid")
	}
	i, _, err := c.loadIssue(issueRef{owner: owner, repo: name, number: ref.number}, rootKey{repo: repo.ID, number: ref.number})
	if err != nil {
		return connector.Root{}, err
	}
	return c.root(i), nil
}
func (h *handler) ReadRoot(ctx context.Context, p connector.ReadRootParams) (connector.Root, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Root{}, err
	}
	i, _, err := c.load(p.RootKey)
	if err != nil {
		return connector.Root{}, err
	}
	return c.root(i), nil
}
func (h *handler) CompleteRoot(ctx context.Context, p connector.CompleteRootParams) (connector.Root, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Root{}, err
	}
	i, ref, err := c.load(p.RootKey)
	if err != nil {
		return connector.Root{}, err
	}
	if i.State != forgejosdk.StateClosed {
		closed := forgejosdk.StateClosed
		if _, response, e := c.api.EditIssue(ref.owner, ref.repo, ref.number, forgejosdk.EditIssueOption{State: &closed}); e != nil {
			return connector.Root{}, providerFailure(response)
		}
		i, _, err = c.load(p.RootKey)
		if err != nil {
			return connector.Root{}, err
		}
		if i.State != forgejosdk.StateClosed {
			return connector.Root{}, failure("provider_error", "Forgejo completion readback did not match")
		}
	}
	return c.root(i), nil
}
