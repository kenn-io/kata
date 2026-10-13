package forgejo

import (
	"context"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}
type label struct {
	ID int64 `json:"id"`
}
type issue struct {
	ID          int64       `json:"id"`
	Number      int64       `json:"number"`
	Title       string      `json:"title"`
	Body        string      `json:"body"`
	State       string      `json:"state"`
	UpdatedAt   time.Time   `json:"updated_at"`
	User        *user       `json:"user"`
	Repository  *repository `json:"repository"`
	PullRequest any         `json:"pull_request"`
	DueDate     *time.Time  `json:"due_date"`
	Labels      []label     `json:"labels"`
	Assignees   []user      `json:"assignees"`
	Milestone   *label      `json:"milestone"`
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
func repoPath(name string) (string, bool) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.TrimSpace(part) != part {
			return "", false
		}
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
				return "", false
			}
		}
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), true
}
func (c *client) locator(s string) (string, int64, *connector.Error) {
	var name, number string
	if strings.Contains(s, "://") {
		u, e := url.Parse(s)
		if e != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || strings.Contains(s, "#") || u.RawPath != "" || normalizedOrigin(u) != c.origin {
			return "", 0, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
		p := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(p) != 4 || p[2] != "issues" {
			return "", 0, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
		name = p[0] + "/" + p[1]
		number = p[3]
	} else {
		var ok bool
		name, number, ok = strings.Cut(s, "#")
		if !ok {
			return "", 0, failure("invalid_locator", "Forgejo issue locator is invalid")
		}
	}
	path, ok := repoPath(name)
	n, valid := numeric(number)
	if !ok || !valid {
		return "", 0, failure("invalid_locator", "Forgejo issue locator is invalid")
	}
	return path, n, nil
}
func (c *client) load(ctx context.Context, key string) (issue, string, *connector.Error) {
	k, ok := parseKey(key)
	if !ok {
		return issue{}, "", failure("invalid_root", "Forgejo root key is invalid")
	}
	var repo repository
	if err := c.request(ctx, "GET", "/repositories/"+id(k.repo), nil, &repo); err != nil {
		if err.Code == "not_found" {
			err.Code = "root_not_found"
		}
		return issue{}, "", err
	}
	path, ok := repoPath(repo.FullName)
	if !ok || repo.ID != k.repo {
		return issue{}, "", failure("identity_mismatch", "Forgejo repository identity changed")
	}
	return c.loadIssue(ctx, path, k)
}
func (c *client) loadIssue(ctx context.Context, path string, k rootKey) (issue, string, *connector.Error) {
	var i issue
	endpoint := path + "/issues/" + id(k.number)
	if err := c.request(ctx, "GET", endpoint, nil, &i); err != nil {
		if err.Code == "not_found" {
			err.Code = "root_not_found"
		}
		return i, "", err
	}
	if i.ID <= 0 || i.Number != k.number || (k.issue != 0 && i.ID != k.issue) || i.Repository == nil || i.Repository.ID != k.repo {
		return i, "", failure("identity_mismatch", "Forgejo issue identity changed")
	}
	if i.PullRequest != nil {
		return i, "", failure("unsupported", "Forgejo pull requests are unsupported")
	}
	if i.State != "open" && i.State != "closed" {
		return i, "", failure("provider_error", "Forgejo issue state is invalid")
	}
	if _, err := fieldValues(i); err != nil {
		return i, "", err
	}
	return i, endpoint, nil
}
func (c *client) root(i issue) connector.Root {
	state := "open"
	if i.State == "closed" {
		state = "complete"
	}
	root := connector.Root{Key: id(i.Repository.ID) + "#" + id(i.Number) + "@" + id(i.ID), IdentityKey: c.origin, Title: i.Title, Body: i.Body, State: state, UpdatedAt: i.UpdatedAt, ObservedAt: time.Now().UTC()}
	root.Fields, _ = fieldValues(i)
	observed := root.ObservedAt
	root.ObservedAt = time.Time{}
	root.Revision = digest(root)
	root.ObservedAt = observed
	return root
}
func (h *handler) ResolveRoot(ctx context.Context, p connector.ResolveRootParams) (connector.Root, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.Root{}, err
	}
	path, n, err := c.locator(p.Locator)
	if err != nil {
		return connector.Root{}, err
	}
	var repo repository
	if err := c.request(ctx, "GET", path, nil, &repo); err != nil {
		if err.Code == "not_found" {
			err.Code = "root_not_found"
		}
		return connector.Root{}, err
	}
	path, ok := repoPath(repo.FullName)
	if !ok || repo.ID <= 0 {
		return connector.Root{}, failure("provider_error", "Forgejo repository is invalid")
	}
	i, _, err := c.loadIssue(ctx, path, rootKey{repo: repo.ID, number: n})
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
	i, _, err := c.load(ctx, p.RootKey)
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
	i, path, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.Root{}, err
	}
	if i.State != "closed" {
		if err := c.request(ctx, "PATCH", path, map[string]string{"state": "closed"}, nil); err != nil {
			return connector.Root{}, err
		}
		i, _, err = c.load(ctx, p.RootKey)
		if err != nil {
			return connector.Root{}, err
		}
		if i.State != "closed" {
			return connector.Root{}, failure("provider_error", "Forgejo completion readback did not match")
		}
	}
	return c.root(i), nil
}
