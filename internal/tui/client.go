package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"

	"go.kenn.io/kata/internal/config"
)

// Client is the typed adapter the TUI uses to talk to the daemon. Errors
// include the request method+path so toast messages stay actionable.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient wraps a pre-built *http.Client with a typed daemon adapter.
// base is the daemon URL — "http://kata.invalid" for unix-socket transport.
func NewClient(base string, hc *http.Client) *Client { return &Client{base: base, hc: hc} }

// GetInstance returns the daemon instance identity and schema version.
func (c *Client) GetInstance(ctx context.Context) (InstanceInfo, error) {
	var resp InstanceInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/instance", nil, &resp)
	return resp, err
}

// ListIssues returns the issues for projectID filtered by f.
func (c *Client) ListIssues(ctx context.Context, projectID int64, f ListFilter) ([]Issue, error) {
	return c.listIssuesAt(ctx, fmt.Sprintf("/api/v1/projects/%d/issues", projectID), f)
}

// ListAllIssues lists issues across every project. The daemon may not yet
// implement /api/v1/issues; in that case the request surfaces as a 404
// APIError that callers can downgrade.
func (c *Client) ListAllIssues(ctx context.Context, f ListFilter) ([]Issue, error) {
	return c.listIssuesAt(ctx, "/api/v1/issues", f)
}

func (c *Client) listIssuesAt(ctx context.Context, path string, f ListFilter) ([]Issue, error) {
	if vals := f.values().Encode(); vals != "" {
		path += "?" + vals
	}
	var resp struct {
		Issues []Issue `json:"issues"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Issues, nil
}

// GetIssueDetail fetches a single issue plus hierarchy metadata by ref.
// ref is a short_id, qualified short_id, or UID — the daemon's path
// resolver picks the matching column.
func (c *Client) GetIssueDetail(ctx context.Context, projectID int64, ref string) (*IssueDetail, error) {
	body, err := c.showIssue(ctx, projectID, ref)
	if err != nil {
		return nil, err
	}
	return &IssueDetail{
		Issue:    &body.Issue,
		Parent:   body.Parent,
		Children: body.Children,
	}, nil
}

// CreateIssue posts a new issue. body.IdempotencyKey rides the
// Idempotency-Key header per spec §4.4 when non-empty.
func (c *Client) CreateIssue(
	ctx context.Context, projectID int64, body CreateIssueBody,
) (*MutationResp, error) {
	headers := map[string]string{}
	if body.IdempotencyKey != "" {
		headers["Idempotency-Key"] = body.IdempotencyKey
	}
	var resp MutationResp
	path := fmt.Sprintf("/api/v1/projects/%d/issues", projectID)
	if err := c.doWithHeaders(ctx, http.MethodPost, path, body, &resp, headers); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Close transitions the issue to status=closed. source=tui signals the
// daemon to skip the substance and evidence checks that gate CLI-side
// closes; the structural guards (parent-close completeness, sibling
// throttle, repeated-message) still apply.
func (c *Client) Close(
	ctx context.Context, projectID int64, ref, actor string,
) (*MutationResp, error) {
	body := map[string]string{"actor": actor, "source": "tui", "reason": "done"}
	return c.mutate(ctx, http.MethodPost,
		issuePath(projectID, ref)+"/actions/close", body)
}

// Reopen transitions the issue back to status=open.
func (c *Client) Reopen(
	ctx context.Context, projectID int64, ref, actor string,
) (*MutationResp, error) {
	return c.mutate(ctx, http.MethodPost,
		issuePath(projectID, ref)+"/actions/reopen", actorBody(actor))
}

// AddComment appends a new comment to the issue.
func (c *Client) AddComment(
	ctx context.Context, projectID int64, ref, body, actor string,
) (*MutationResp, error) {
	return c.mutate(ctx, http.MethodPost, issuePath(projectID, ref)+"/comments",
		map[string]string{"body": body, "actor": actor})
}

// AddLabel attaches a label to the issue.
func (c *Client) AddLabel(
	ctx context.Context, projectID int64, ref, label, actor string,
) (*MutationResp, error) {
	return c.mutate(ctx, http.MethodPost, issuePath(projectID, ref)+"/labels",
		map[string]string{"label": label, "actor": actor})
}

// RemoveLabel sends actor in the query string because DELETE bodies are
// non-portable; the label is path-escaped to survive '/' and similar.
func (c *Client) RemoveLabel(
	ctx context.Context, projectID int64, ref, label, actor string,
) (*MutationResp, error) {
	path := fmt.Sprintf("%s/labels/%s?actor=%s",
		issuePath(projectID, ref), url.PathEscape(label), url.QueryEscape(actor))
	return c.mutate(ctx, http.MethodDelete, path, nil)
}

// Assign sets the issue owner. Empty owner routes to /actions/unassign
// because the daemon's PATCH endpoint cannot represent the clear case
// (string vs null) and /actions/assign rejects empty owners with 400.
func (c *Client) Assign(
	ctx context.Context, projectID int64, ref, owner, actor string,
) (*MutationResp, error) {
	if owner == "" {
		return c.mutate(ctx, http.MethodPost,
			issuePath(projectID, ref)+"/actions/unassign", actorBody(actor))
	}
	return c.mutate(ctx, http.MethodPost,
		issuePath(projectID, ref)+"/actions/assign",
		map[string]string{"owner": owner, "actor": actor})
}

// SetPriority sends the issue's priority through /actions/priority. A
// nil priority clears the field. Mirrors Assign's pattern of routing
// the optional/clear case through the same endpoint with a nil body
// field; the daemon distinguishes set-vs-clear from the JSON shape.
func (c *Client) SetPriority(
	ctx context.Context, projectID int64, ref string, priority *int64, actor string,
) (*MutationResp, error) {
	body := map[string]any{"actor": actor}
	if priority != nil {
		body["priority"] = *priority
	}
	return c.mutate(ctx, http.MethodPost,
		issuePath(projectID, ref)+"/actions/priority", body)
}

// AddLink creates a typed link from this issue to body.ToRef. The
// daemon's CreateLinkRequest.Body is {actor, type, to_ref}; ToRef
// accepts a short_id, qualified short_id ("kata#abc4"), or a 26-char
// ULID.
func (c *Client) AddLink(
	ctx context.Context, projectID int64, ref string, body LinkBody, actor string,
) (*MutationResp, error) {
	return c.mutate(ctx, http.MethodPost, issuePath(projectID, ref)+"/links",
		map[string]any{"type": body.Type, "to_ref": body.ToRef, "actor": actor})
}

// RemoveLink deletes a link by id. actor rides the query string per the
// DELETE-body portability convention.
func (c *Client) RemoveLink(
	ctx context.Context, projectID int64, ref string, linkID int64, actor string,
) (*MutationResp, error) {
	path := fmt.Sprintf("%s/links/%d?actor=%s",
		issuePath(projectID, ref), linkID, url.QueryEscape(actor))
	return c.mutate(ctx, http.MethodDelete, path, nil)
}

// EditBody replaces issue.body via PATCH. v1 only supports body edits
// from the TUI; title edits would reuse the same endpoint.
func (c *Client) EditBody(
	ctx context.Context, projectID int64, ref, body, actor string,
) (*MutationResp, error) {
	return c.mutate(ctx, http.MethodPatch, issuePath(projectID, ref),
		map[string]any{"body": body, "actor": actor})
}

// ResolveProject runs the §4.2 resolution flow against startPath.
//
// Wire shape is chosen client-side so the daemon never has to stat the
// client's filesystem (issue #35): {name, alias?} when .kata.toml is
// readable, {alias} for a git workspace without .kata.toml, and
// {start_path} as a legacy local-only fallback. When the daemon
// returns a canonical name that differs from the local .kata.toml, the
// client rewrites the file in place.
func (c *Client) ResolveProject(ctx context.Context, startPath string) (*ResolveResp, error) {
	req, repair, err := buildResolveRequest(ctx, startPath)
	if err != nil {
		return nil, err
	}
	var resp ResolveResp
	if err := c.do(ctx, http.MethodPost, "/api/v1/projects/resolve", req, &resp); err != nil {
		return nil, err
	}
	if repair != nil {
		if err := repair(resp.Project.Name); err != nil {
			return nil, err
		}
	}
	return &resp, nil
}

// buildResolveRequest selects the resolve wire shape and returns an
// optional callback that rewrites .kata.toml when the daemon's
// canonical name differs from the local binding.
func buildResolveRequest(ctx context.Context, startPath string) (map[string]any, func(string) error, error) {
	disc, err := config.DiscoverPaths(startPath)
	if err != nil {
		// Path doesn't exist locally — pass through to start_path so
		// the daemon's not-initialized error surfaces uniformly, same
		// as the pre-12ced3a behavior. Permission and other stat
		// errors still propagate (they indicate a real problem the
		// user needs to know about).
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"start_path": startPath}, nil, nil
		}
		return nil, nil, err
	}
	var tomlCfg *config.ProjectConfig
	if disc.WorkspaceRoot != "" {
		cfg, err := config.ReadProjectConfig(disc.WorkspaceRoot)
		switch {
		case err == nil:
			tomlCfg = cfg
		case errors.Is(err, config.ErrProjectConfigMissing):
			// no .kata.toml
		default:
			// Surface the parse error directly so the user sees the
			// fix-it message instead of a confusing daemon-side stat
			// failure under remote-client mode.
			return nil, nil, fmt.Errorf("read .kata.toml: %w", err)
		}
	}

	var alias *config.AliasInfo
	if disc.GitRoot != "" || disc.WorkspaceRoot != "" {
		info, derr := config.ComputeAliasIdentity(ctx, disc)
		switch {
		case derr == nil:
			alias = &info
		case tomlCfg == nil:
			return nil, nil, derr
		}
	}

	body := map[string]any{}
	if tomlCfg != nil && tomlCfg.Project.Name != "" {
		body["name"] = tomlCfg.Project.Name
		if alias != nil {
			body["alias"] = aliasInputBody(*alias)
		}
		workspaceRoot := disc.WorkspaceRoot
		current := tomlCfg.Project.Name
		repair := func(canonical string) error {
			if canonical == "" || canonical == current {
				return nil
			}
			if err := config.WriteProjectConfig(workspaceRoot, canonical); err != nil {
				return fmt.Errorf("rewrite .kata.toml: %w", err)
			}
			return nil
		}
		return body, repair, nil
	}

	if alias != nil {
		body["alias"] = aliasInputBody(*alias)
		return body, nil, nil
	}

	body["start_path"] = startPath
	return body, nil, nil
}

// aliasInputBody marshals a config.AliasInfo into the api.AliasInput
// wire shape (untyped map to keep the daemon-facing JSON local to
// this file).
func aliasInputBody(info config.AliasInfo) map[string]any {
	return map[string]any{
		"identity": info.Identity,
		"kind":     info.Kind,
	}
}

// ListLabels returns the per-label aggregate counts for projectID. The
// daemon's GET /api/v1/projects/{id}/labels endpoint backs the +
// suggestion menu; counts drive the "most-used first" sort.
func (c *Client) ListLabels(ctx context.Context, projectID int64) ([]LabelCount, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/labels", projectID)
	var resp struct {
		Labels []LabelCount `json:"labels"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Labels, nil
}

// ListProjects returns the daemon's known projects.
func (c *Client) ListProjects(ctx context.Context) ([]ProjectSummary, error) {
	var resp struct {
		Projects []ProjectSummary `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// EnsureProject creates or returns a project by name on the target daemon.
func (c *Client) EnsureProject(ctx context.Context, name string) (ProjectSummary, error) {
	var resp struct {
		Project ProjectSummary `json:"project"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/projects", map[string]string{"name": name}, &resp)
	return resp.Project, err
}

// FederationStatus returns redacted federation status for all local bindings.
func (c *Client) FederationStatus(ctx context.Context) (FederationStatusBody, error) {
	var resp FederationStatusBody
	err := c.do(ctx, http.MethodGet, "/api/v1/federation/status", nil, &resp)
	return resp, err
}

// EnableFederation enables federation metadata for an existing project.
func (c *Client) EnableFederation(
	ctx context.Context,
	projectID int64,
	actor string,
) (ProjectFederationMetadata, error) {
	var resp ProjectFederationMetadata
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%d/federation/enable", projectID),
		map[string]string{"actor": actor}, &resp)
	return resp, err
}

// CreateFederationEnrollment creates a temporary hub enrollment token.
func (c *Client) CreateFederationEnrollment(
	ctx context.Context,
	body CreateFederationEnrollmentInput,
) (FederationEnrollment, error) {
	var resp FederationEnrollment
	err := c.do(ctx, http.MethodPost, "/api/v1/federation/enrollments", body, &resp)
	return resp, err
}

// CreateFederationReplica joins or adopts a local project as a spoke.
func (c *Client) CreateFederationReplica(
	ctx context.Context,
	body CreateFederationReplicaInput,
) (FederationReplicaResult, error) {
	var resp FederationReplicaResult
	err := c.do(ctx, http.MethodPost, "/api/v1/federation/replicas", body, &resp)
	return resp, err
}

// ListProjectsWithStats returns every active project with per-project
// aggregates {open, closed, last_event_at} populated. Used by the
// projects view. Spec §7.3.
func (c *Client) ListProjectsWithStats(ctx context.Context) ([]ProjectSummaryWithStats, error) {
	var resp struct {
		Projects []ProjectSummaryWithStats `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects?include=stats", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Projects == nil {
		resp.Projects = []ProjectSummaryWithStats{}
	}
	return resp.Projects, nil
}

// ListComments and ListLinks route through GET /issues/{ref} because
// the daemon embeds both slices there. ListEvents filters client-side
// because the poll endpoint accepts no issue-targeted query filter.
func (c *Client) ListComments(
	ctx context.Context, projectID int64, ref string,
) ([]CommentEntry, error) {
	body, err := c.showIssue(ctx, projectID, ref)
	if err != nil {
		return nil, err
	}
	return body.Comments, nil
}

// ListEvents returns the events tab data for one issue. See above note
// on the client-side filter. ref is the issue's short_id; the daemon's
// event stream embeds issue_short_id on every issue-scoped event, so
// the filter is project-local and stable for the life of the daemon.
//
// TODO(plan-6/task-8): the 200-event window is a one-shot snapshot;
// full pagination via next_after_id is deferred. The poll envelope's
// reset_required is decoded but ignored here because Task 8 fetches
// once per detail-view open; the SSE consumer (Task 11) handles
// reset_required for the long-lived stream.
func (c *Client) ListEvents(ctx context.Context, projectID int64, ref string) ([]EventLogEntry, error) {
	var out []EventLogEntry
	afterID := int64(0)
	for {
		resp, err := c.listEventsPage(ctx, projectID, afterID)
		if err != nil {
			return nil, err
		}
		for _, e := range resp.Events {
			if ref != "" && e.IssueShortID != nil && *e.IssueShortID == ref {
				out = append(out, e)
			}
		}
		if resp.ResetRequired || len(resp.Events) == 0 || resp.NextAfterID <= afterID {
			return out, nil
		}
		afterID = resp.NextAfterID
	}
}

type eventsPageResp struct {
	Events        []EventLogEntry `json:"events"`
	NextAfterID   int64           `json:"next_after_id"`
	ResetRequired bool            `json:"reset_required"`
	ResetAfterID  int64           `json:"reset_after_id,omitempty"`
}

func (c *Client) listEventsPage(ctx context.Context, projectID, afterID int64) (eventsPageResp, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/events?limit=1000", projectID)
	if afterID > 0 {
		path += fmt.Sprintf("&after_id=%d", afterID)
	}
	var resp eventsPageResp
	err := c.do(ctx, http.MethodGet, path, nil, &resp)
	return resp, err
}

// ListLinks returns the links tab data for one issue.
func (c *Client) ListLinks(ctx context.Context, projectID int64, ref string) ([]LinkEntry, error) {
	body, err := c.showIssue(ctx, projectID, ref)
	if err != nil {
		return nil, err
	}
	return body.Links, nil
}

func (c *Client) showIssue(ctx context.Context, projectID int64, ref string) (*showIssueBody, error) {
	var resp showIssueBody
	if err := c.do(ctx, http.MethodGet, issuePath(projectID, ref), nil, &resp); err != nil {
		return nil, err
	}
	// Lift the sibling labels slice onto resp.Issue.Labels so detail
	// rendering reads a single field. Sort alphabetically so the wire
	// order (insertion-order from the daemon) doesn't bleed into the
	// chip rendering — list-row decode produces sorted labels too,
	// so the detail and list paths agree.
	if len(resp.Labels) > 0 {
		labels := make([]string, len(resp.Labels))
		for i, lbl := range resp.Labels {
			labels[i] = lbl.Label
		}
		sort.Strings(labels)
		resp.Issue.Labels = labels
	}
	for i := range resp.Children {
		sort.Strings(resp.Children[i].Labels)
	}
	return &resp, nil
}

func issuePath(projectID int64, ref string) string {
	return fmt.Sprintf("/api/v1/projects/%d/issues/%s", projectID, url.PathEscape(ref))
}

func actorBody(actor string) map[string]string { return map[string]string{"actor": actor} }

// mutate is the shared shape of every mutation method: encode body,
// dispatch, decode the §4.5 envelope.
func (c *Client) mutate(ctx context.Context, method, path string, body any) (*MutationResp, error) {
	var resp MutationResp
	if err := c.do(ctx, method, path, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithHeaders(ctx, method, path, body, out, nil)
}

func (c *Client) doWithHeaders(
	ctx context.Context, method, path string, body, out any, headers map[string]string,
) error {
	req, err := buildRequest(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req) //nolint:gosec // G704: c.base built from our own daemon discovery
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return decodeError(resp, method, path)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func buildRequest(ctx context.Context, method, fullURL string, body any) (*http.Request, error) {
	if body == nil {
		return http.NewRequestWithContext(ctx, method, fullURL, nil)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func decodeError(resp *http.Response, method, path string) error {
	var env struct {
		Status int `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return &APIError{
		Method:  method,
		Path:    path,
		Status:  resp.StatusCode,
		Code:    env.Error.Code,
		Message: env.Error.Message,
		Hint:    env.Error.Hint,
	}
}
