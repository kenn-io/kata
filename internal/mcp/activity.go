package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	oapiruntime "github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/pkg/client/generated"
)

// NextInput selects the highest-priority ready issue.
type NextInput struct {
	Project       string   `json:"project,omitempty"`
	Owner         string   `json:"owner,omitempty"`
	Unowned       bool     `json:"unowned,omitempty"`
	Labels        []string `json:"labels,omitempty"`
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
}

// NextOutput contains the selected issue when one is ready.
type NextOutput struct {
	Issue *IssueSummary `json:"issue,omitempty"`
}

// EditCommentInput identifies a comment and its replacement body.
type EditCommentInput struct {
	Ref        string `json:"ref"`
	CommentRef string `json:"comment_ref"`
	Body       string `json:"body"`
}

// EditCommentOutput reports the edited comment and issue state.
type EditCommentOutput struct {
	Project ProjectIdentity `json:"project"`
	Issue   IssueSummary    `json:"issue"`
	Comment CommentSummary  `json:"comment"`
	Changed bool            `json:"changed"`
	Event   *EventSummary   `json:"event,omitempty"`
}

// GraphInput selects a bounded relationship graph.
type GraphInput struct {
	Ref      string `json:"ref"`
	Depth    string `json:"depth,omitempty"`
	HideDone bool   `json:"hide_done,omitempty"`
}

// GraphOutput contains the reachable issue graph.
type GraphOutput struct {
	Project        ProjectIdentity                         `json:"project"`
	SourceUID      string                                  `json:"source_uid"`
	Depth          string                                  `json:"depth"`
	HideDone       bool                                    `json:"hide_done"`
	Nodes          []IssueSummary                          `json:"nodes"`
	Edges          []generated.ReachableGraphEdge          `json:"edges"`
	UnresolvedRefs []generated.ReachableGraphUnresolvedRef `json:"unresolved_refs,omitempty"`
}

// MoveInput selects an issue and destination project.
type MoveInput struct {
	Ref       string `json:"ref"`
	ToProject string `json:"to_project"`
	Revision  *int64 `json:"revision,omitempty"`
}

// MoveOutput reports the issue after a project move.
type MoveOutput struct {
	From       ProjectIdentity `json:"from"`
	To         ProjectIdentity `json:"to"`
	Issue      IssueSummary    `json:"issue"`
	Changed    bool            `json:"changed"`
	EventID    int64           `json:"event_id"`
	NewShortID string          `json:"new_short_id"`
}

// DeleteInput carries a soft-delete confirmation.
type DeleteInput struct {
	Ref     string `json:"ref"`
	Confirm string `json:"confirm"`
	Reason  string `json:"reason,omitempty"`
}

// RestoreInput identifies a soft-deleted issue to restore.
type RestoreInput struct {
	Ref string `json:"ref"`
}

// PurgeInput carries an irreversible purge confirmation.
type PurgeInput struct {
	Ref     string `json:"ref"`
	Confirm string `json:"confirm"`
	Reason  string `json:"reason,omitempty"`
}

// PurgeOutput reports a completed issue purge.
type PurgeOutput struct {
	Project ProjectIdentity    `json:"project"`
	Ref     string             `json:"ref"`
	Purged  bool               `json:"purged"`
	Log     generated.PurgeLog `json:"log"`
}

// WaitInput defines bounded issue-state wait conditions.
type WaitInput struct {
	Refs           []string `json:"refs"`
	Status         string   `json:"status,omitempty"`
	Any            bool     `json:"any,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// WaitState reports one observed issue state.
type WaitState struct {
	Ref     string `json:"ref"`
	Status  string `json:"status"`
	Matched bool   `json:"matched"`
}

// WaitOutput reports why an issue-state wait ended.
type WaitOutput struct {
	Reason string      `json:"reason"`
	States []WaitState `json:"states"`
}

// AuditClosesInput filters close audit records.
type AuditClosesInput struct {
	Project    string `json:"project,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Actor      string `json:"actor,omitempty"`
	Parent     string `json:"parent,omitempty"`
	Reason     string `json:"reason,omitempty"`
	NoEvidence bool   `json:"no_evidence,omitempty"`
}

// AuditClosesOutput contains close audit records for one project.
type AuditClosesOutput struct {
	Project ProjectIdentity           `json:"project"`
	Rows    []generated.AuditCloseRow `json:"rows"`
}

// DigestInput defines an activity digest window.
type DigestInput struct {
	Project string   `json:"project,omitempty"`
	Since   string   `json:"since"`
	Until   string   `json:"until,omitempty"`
	Actors  []string `json:"actors,omitempty"`
}

// ProjectDigest associates one digest with its project scope.
type ProjectDigest struct {
	Project *ProjectIdentity             `json:"project,omitempty"`
	Digest  generated.DigestResponseBody `json:"digest"`
}

// DigestOutput contains one or more scoped digests.
type DigestOutput struct {
	Digests []ProjectDigest `json:"digests"`
}

// EventsInput defines an immediate poll or bounded stream wait.
type EventsInput struct {
	Project     string `json:"project,omitempty"`
	Mode        string `json:"mode,omitempty"`
	After       int64  `json:"after,omitempty"`
	Limit       int64  `json:"limit,omitempty"`
	WaitSeconds int64  `json:"wait_seconds,omitempty"`
}

// EventsOutput contains events and the next resume cursor.
type EventsOutput struct {
	Events        []StreamEvent `json:"events"`
	NextAfterID   int64         `json:"next_after_id"`
	ResetRequired bool          `json:"reset_required"`
	ResetAfterID  *int64        `json:"reset_after_id,omitempty"`
	TimedOut      bool          `json:"timed_out,omitempty"`
}

// StreamEvent is the MCP-safe event envelope.
type StreamEvent struct {
	EventID             int64  `json:"event_id"`
	EventUID            string `json:"event_uid"`
	Type                string `json:"type"`
	Actor               string `json:"actor"`
	CreatedAt           string `json:"created_at"`
	ProjectID           int64  `json:"project_id"`
	ProjectUID          string `json:"project_uid"`
	ProjectName         string `json:"project_name"`
	IssueUID            string `json:"issue_uid,omitempty"`
	IssueShortID        string `json:"issue_short_id,omitempty"`
	RelatedIssueUID     string `json:"related_issue_uid,omitempty"`
	RelatedIssueShortID string `json:"related_issue_short_id,omitempty"`
	Payload             any    `json:"payload,omitempty"`
}

func registerActivityTools(server *sdkmcp.Server, handlers toolHandlers) {
	read := toolHints(true, false, false)
	addTool(server, "kata.digest", "Activity digest", "Read an actor-grouped activity digest for an explicit time window.", read, handlers.digest)
	addTool(server, "kata.events", "Read events", "Poll events or wait on the live event stream with a resumable cursor.", read, handlers.events)
}

func (h toolHandlers) next(ctx context.Context, _ *sdkmcp.CallToolRequest, input NextInput) (*sdkmcp.CallToolResult, NextOutput, error) {
	issues, err := h.allReadyIssues(ctx, input)
	if err != nil {
		return nil, NextOutput{}, err
	}
	if len(issues) == 0 {
		return successResult(), NextOutput{}, nil
	}
	selected := issues[0]
	for _, candidate := range issues[1:] {
		if selected.Priority == nil && candidate.Priority != nil ||
			selected.Priority != nil && candidate.Priority != nil && *candidate.Priority < *selected.Priority {
			selected = candidate
		}
	}
	return successResult(), NextOutput{Issue: &selected}, nil
}

func (h toolHandlers) allReadyIssues(ctx context.Context, input NextInput) ([]IssueSummary, error) {
	if input.Unowned && strings.TrimSpace(input.Owner) != "" {
		return nil, errors.New("owner and unowned are mutually exclusive")
	}
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, err
	}
	issues := make([]IssueSummary, 0)
	query := generated.ReadyIssuesQuery{
		Unowned: optionalTrue(input.Unowned), Owner: optionalString(input.Owner),
		Label: compactStrings(input.Labels), ExcludeLabel: compactStrings(input.ExcludeLabels),
	}
	if h.options.Scope.Mode() == ScopeAllowlist && strings.TrimSpace(input.Project) == "" {
		for _, project := range projects {
			response, readyErr := h.options.Client.ReadyIssues(ctx, &generated.ReadyIssuesRequestOptions{
				PathParams: &generated.ReadyIssuesPath{ProjectID: project.ID}, Query: &query,
			})
			if readyErr != nil {
				return nil, readyErr
			}
			for _, issue := range response.Issues {
				issues = append(issues, h.summaryFromIssueOut(project, issue))
			}
		}
		sortIssueSummaries(issues)
		return issues, nil
	}
	if len(projects) == 1 && (h.options.Scope.Mode() == ScopeBound || strings.TrimSpace(input.Project) != "") {
		response, readyErr := h.options.Client.ReadyIssues(ctx, &generated.ReadyIssuesRequestOptions{
			PathParams: &generated.ReadyIssuesPath{ProjectID: projects[0].ID}, Query: &query,
		})
		if readyErr != nil {
			return nil, readyErr
		}
		for _, issue := range response.Issues {
			issues = append(issues, h.summaryFromIssueOut(projects[0], issue))
		}
		return issues, nil
	}
	response, err := h.options.Client.ReadyIssuesGlobal(ctx, &generated.ReadyIssuesGlobalRequestOptions{
		Query: &generated.ReadyIssuesGlobalQuery{
			Unowned: query.Unowned, Owner: query.Owner, Label: query.Label, ExcludeLabel: query.ExcludeLabel,
		},
	})
	if err != nil {
		return nil, err
	}
	allowed := projectIDSet(projects)
	for _, issue := range response.Issues {
		if _, ok := allowed[issue.ProjectID]; ok {
			issues = append(issues, summaryFromReadyGlobalIssue(issue))
		}
	}
	return issues, nil
}

func (h toolHandlers) editComment(ctx context.Context, _ *sdkmcp.CallToolRequest, input EditCommentInput) (*sdkmcp.CallToolResult, EditCommentOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, EditCommentOutput{}, err
	}
	if strings.TrimSpace(input.CommentRef) == "" || strings.TrimSpace(input.Body) == "" {
		return nil, EditCommentOutput{}, errors.New("comment_ref and body are required")
	}
	response, err := h.options.Client.EditComment(ctx, &generated.EditCommentRequestOptions{
		PathParams: &generated.EditCommentPath{ProjectID: project.ID, Ref: ref, CommentRef: input.CommentRef},
		Body:       &generated.EditCommentBody{Actor: h.options.Actor, Body: input.Body},
	})
	if err != nil {
		return nil, EditCommentOutput{}, err
	}
	return successResult(), EditCommentOutput{
		Project: project, Issue: h.summaryFromIssue(project, response.Issue),
		Comment: commentSummary(response.Comment), Changed: response.Changed, Event: eventSummary(&response.Event),
	}, nil
}

func (h toolHandlers) graph(ctx context.Context, _ *sdkmcp.CallToolRequest, input GraphInput) (*sdkmcp.CallToolResult, GraphOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, false)
	if err != nil {
		return nil, GraphOutput{}, err
	}
	depth := strings.TrimSpace(input.Depth)
	if depth == "" {
		depth = "full"
	}
	response, err := h.options.Client.ReachableIssueGraph(ctx, &generated.ReachableIssueGraphRequestOptions{
		PathParams: &generated.ReachableIssueGraphPath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.ReachableIssueGraphQuery{Depth: &depth, HideDone: optionalTrue(input.HideDone)},
	})
	if err != nil {
		return nil, GraphOutput{}, err
	}
	allowedProjects := map[int64]struct{}{}
	if h.options.Scope.Mode() != ScopeAll {
		projects, scopeErr := h.options.Scope.Projects(ctx, h.options.Client, false)
		if scopeErr != nil {
			return nil, GraphOutput{}, scopeErr
		}
		allowedProjects = projectIDSet(projects)
	}
	nodes := make([]IssueSummary, 0, len(response.Nodes))
	allowedUIDs := make(map[string]struct{}, len(response.Nodes))
	for _, node := range response.Nodes {
		if h.options.Scope.Mode() != ScopeAll {
			if _, ok := allowedProjects[node.ProjectID]; !ok {
				continue
			}
		}
		allowedUIDs[node.UID] = struct{}{}
		nodes = append(nodes, IssueSummary{
			UID: node.UID, Ref: node.ShortID, QualifiedRef: node.QualifiedID, Title: node.Title,
			Status: node.Status, Owner: node.Owner, Priority: node.Priority, Revision: node.Revision,
			UpdatedAt: formatTime(node.UpdatedAt), ScheduledOn: metadataString(node.Metadata, "scheduled_on"),
			Timezone: metadataString(node.Metadata, "timezone"),
		})
	}
	edges := make([]generated.ReachableGraphEdge, 0, len(response.Edges))
	for _, edge := range response.Edges {
		_, fromAllowed := allowedUIDs[edge.FromUID]
		_, toAllowed := allowedUIDs[edge.ToUID]
		if h.options.Scope.Mode() == ScopeAll || fromAllowed && toAllowed {
			edges = append(edges, edge)
		}
	}
	unresolved := make([]generated.ReachableGraphUnresolvedRef, 0, len(response.UnresolvedRefs))
	for _, ref := range response.UnresolvedRefs {
		_, uidAllowed := allowedUIDs[ref.UID]
		_, otherAllowed := allowedUIDs[ref.OtherUID]
		if h.options.Scope.Mode() == ScopeAll || uidAllowed && otherAllowed {
			unresolved = append(unresolved, ref)
		}
	}
	return successResult(), GraphOutput{
		Project: project, SourceUID: response.SourceUID, Depth: response.Depth, HideDone: response.HideDone,
		Nodes: nodes, Edges: edges, UnresolvedRefs: unresolved,
	}, nil
}

func (h toolHandlers) move(ctx context.Context, _ *sdkmcp.CallToolRequest, input MoveInput) (*sdkmcp.CallToolResult, MoveOutput, error) {
	from, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MoveOutput{}, err
	}
	to, err := h.options.Scope.Project(ctx, h.options.Client, input.ToProject, false)
	if err != nil {
		return nil, MoveOutput{}, err
	}
	if to.UID == "" {
		return nil, MoveOutput{}, errors.New("target project UID is required")
	}
	revision := input.Revision
	if revision == nil {
		shown, showErr := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{PathParams: &generated.ShowIssuePath{ProjectID: from.ID, Ref: ref}})
		if showErr != nil {
			return nil, MoveOutput{}, showErr
		}
		revision = &shown.Issue.Revision
	}
	ifMatch := fmt.Sprintf(`"rev-%d"`, *revision)
	response, err := h.options.Client.MoveIssue(ctx, &generated.MoveIssueRequestOptions{
		PathParams: &generated.MoveIssuePath{ProjectID: from.ID, Ref: ref},
		Body:       &generated.MoveIssueBody{Actor: &h.options.Actor, ToProjectUID: to.UID},
		Header:     &generated.MoveIssueHeaders{IfMatch: &ifMatch},
	})
	if err != nil {
		return nil, MoveOutput{}, err
	}
	return successResult(), MoveOutput{
		From: from, To: to, Issue: h.summaryFromIssue(to, response.Issue), Changed: response.Changed,
		EventID: response.EventID, NewShortID: response.NewShortID,
	}, nil
}

func (h toolHandlers) deleteIssue(ctx context.Context, _ *sdkmcp.CallToolRequest, input DeleteInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, qualified, err := h.destructiveTarget(ctx, input.Ref, input.Confirm, "DELETE")
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.DeleteIssue(ctx, &generated.DeleteIssueRequestOptions{
		PathParams: &generated.DeleteIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.DeleteIssueBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)},
		Header:     &generated.DeleteIssueHeaders{XKataConfirm: &qualified},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) restoreIssue(ctx context.Context, _ *sdkmcp.CallToolRequest, input RestoreInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.RestoreIssue(ctx, &generated.RestoreIssueRequestOptions{
		PathParams: &generated.RestoreIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.RestoreIssueBody{Actor: h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) purgeIssue(ctx context.Context, _ *sdkmcp.CallToolRequest, input PurgeInput) (*sdkmcp.CallToolResult, PurgeOutput, error) {
	project, ref, qualified, err := h.destructiveTarget(ctx, input.Ref, input.Confirm, "PURGE")
	if err != nil {
		return nil, PurgeOutput{}, err
	}
	response, err := h.options.Client.PurgeIssue(ctx, &generated.PurgeIssueRequestOptions{
		PathParams: &generated.PurgeIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.PurgeIssueBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)},
		Header:     &generated.PurgeIssueHeaders{XKataConfirm: &qualified},
	})
	if err != nil {
		return nil, PurgeOutput{}, err
	}
	return successResult(), PurgeOutput{Project: project, Ref: project.Name + "#" + ref, Purged: true, Log: response.PurgeLog}, nil
}

func (h toolHandlers) destructiveTarget(ctx context.Context, rawRef, confirm, verb string) (ProjectIdentity, string, string, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, rawRef, true)
	if err != nil {
		return ProjectIdentity{}, "", "", err
	}
	includeDeleted := verb == "PURGE"
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
	})
	if err != nil {
		return ProjectIdentity{}, "", "", err
	}
	canonical := project.Name + "#" + shown.Issue.ShortID
	expected := verb + " " + canonical
	if confirm != expected {
		return ProjectIdentity{}, "", "", fmt.Errorf("confirm must equal %q", expected)
	}
	return project, shown.Issue.ShortID, expected, nil
}

func (h toolHandlers) wait(ctx context.Context, _ *sdkmcp.CallToolRequest, input WaitInput) (*sdkmcp.CallToolResult, WaitOutput, error) {
	if len(input.Refs) == 0 || len(input.Refs) > maximumResultLimit {
		return nil, WaitOutput{}, fmt.Errorf("refs must contain between 1 and %d issue references", maximumResultLimit)
	}
	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = "closed"
	}
	if status != "open" && status != "closed" && status != "deleted" {
		return nil, WaitOutput{}, errors.New("status must be open, closed, or deleted")
	}
	timeout := input.TimeoutSeconds
	if timeout == 0 {
		timeout = 30
	}
	if timeout < 1 || timeout > 300 {
		return nil, WaitOutput{}, errors.New("timeout_seconds must be between 1 and 300")
	}
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	for {
		states := make([]WaitState, 0, len(input.Refs))
		matches := 0
		for _, raw := range input.Refs {
			project, ref, targetErr := h.options.Scope.IssueTarget(waitContext, h.options.Client, raw, false)
			if targetErr != nil {
				return nil, WaitOutput{}, targetErr
			}
			includeDeleted := true
			shown, showErr := h.options.Client.ShowIssue(waitContext, &generated.ShowIssueRequestOptions{
				PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
				Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
			})
			if showErr != nil {
				return nil, WaitOutput{}, showErr
			}
			actual := shown.Issue.Status
			if shown.Issue.DeletedAt != nil {
				actual = "deleted"
			}
			matched := actual == status
			if matched {
				matches++
			}
			states = append(states, WaitState{Ref: project.Name + "#" + shown.Issue.ShortID, Status: actual, Matched: matched})
		}
		if input.Any && matches > 0 || !input.Any && matches == len(states) {
			return successResult(), WaitOutput{Reason: "matched", States: states}, nil
		}
		select {
		case <-waitContext.Done():
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				return successResult(), WaitOutput{Reason: "timeout", States: states}, nil
			}
			return nil, WaitOutput{}, waitContext.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (h toolHandlers) auditCloses(ctx context.Context, _ *sdkmcp.CallToolRequest, input AuditClosesInput) (*sdkmcp.CallToolResult, AuditClosesOutput, error) {
	project, err := h.createTarget(ctx, input.Project)
	if err != nil {
		return nil, AuditClosesOutput{}, err
	}
	response, err := h.options.Client.AuditCloses(ctx, &generated.AuditClosesRequestOptions{Query: &generated.AuditClosesQuery{
		ProjectID: project.ID, Since: optionalString(input.Since), Until: optionalString(input.Until),
		Actor: optionalString(input.Actor), Parent: optionalString(input.Parent), Reason: optionalString(input.Reason),
		NoEvidence: optionalTrue(input.NoEvidence),
	}})
	if err != nil {
		return nil, AuditClosesOutput{}, err
	}
	rows, err := h.redactAuditParentsOutsideScope(ctx, response.Rows)
	if err != nil {
		return nil, AuditClosesOutput{}, err
	}
	return successResult(), AuditClosesOutput{Project: project, Rows: rows}, nil
}

// redactAuditParentsOutsideScope blanks qualified parent references whose
// project qualifier is not in the startup scope. Bare parent refs are
// same-project by the daemon's rendering contract.
func (h toolHandlers) redactAuditParentsOutsideScope(ctx context.Context, rows []generated.AuditCloseRow) ([]generated.AuditCloseRow, error) {
	if h.options.Scope.Mode() == ScopeAll {
		return rows, nil
	}
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, true)
	if err != nil {
		return nil, err
	}
	inScope := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		inScope[project.Name] = struct{}{}
	}
	for index := range rows {
		parent := rows[index].Parent
		if parent == nil {
			continue
		}
		qualifier, _, qualified := strings.Cut(*parent, "#")
		if !qualified {
			continue
		}
		if _, ok := inScope[qualifier]; !ok {
			rows[index].Parent = nil
		}
	}
	return rows, nil
}

func (h toolHandlers) digest(ctx context.Context, _ *sdkmcp.CallToolRequest, input DigestInput) (*sdkmcp.CallToolResult, DigestOutput, error) {
	if strings.TrimSpace(input.Since) == "" {
		return nil, DigestOutput{}, errors.New("since is required")
	}
	if strings.TrimSpace(input.Project) == "" && h.options.Scope.Mode() == ScopeAll {
		response, err := h.options.Client.DigestGlobal(ctx, &generated.DigestGlobalRequestOptions{Query: &generated.DigestGlobalQuery{Since: input.Since, Until: optionalString(input.Until), Actor: input.Actors}})
		if err != nil {
			return nil, DigestOutput{}, err
		}
		return successResult(), DigestOutput{Digests: []ProjectDigest{{Digest: *response}}}, nil
	}
	projects, err := h.activityProjects(ctx, input.Project)
	if err != nil {
		return nil, DigestOutput{}, err
	}
	out := make([]ProjectDigest, 0, len(projects))
	for _, project := range projects {
		response, digestErr := h.options.Client.DigestProject(ctx, &generated.DigestProjectRequestOptions{PathParams: &generated.DigestProjectPath{ProjectID: project.ID}, Query: &generated.DigestProjectQuery{Since: input.Since, Until: optionalString(input.Until), Actor: input.Actors}})
		if digestErr != nil {
			return nil, DigestOutput{}, digestErr
		}
		projectCopy := project
		out = append(out, ProjectDigest{Project: &projectCopy, Digest: *response})
	}
	return successResult(), DigestOutput{Digests: out}, nil
}

func (h toolHandlers) events(ctx context.Context, _ *sdkmcp.CallToolRequest, input EventsInput) (*sdkmcp.CallToolResult, EventsOutput, error) {
	mode := strings.TrimSpace(input.Mode)
	if mode == "" {
		mode = "poll"
	}
	if mode != "poll" && mode != "wait" {
		return nil, EventsOutput{}, errors.New("mode must be poll or wait")
	}
	if input.After < 0 {
		return nil, EventsOutput{}, errors.New("after must not be negative")
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultResultLimit
	}
	if limit < 1 || limit > maximumResultLimit {
		return nil, EventsOutput{}, fmt.Errorf("limit must be between 1 and %d", maximumResultLimit)
	}
	if mode == "poll" {
		output, err := h.pollScopedEvents(ctx, input.Project, input.After, limit)
		if err != nil {
			return nil, EventsOutput{}, err
		}
		return successResult(), output, nil
	}
	waitSeconds := input.WaitSeconds
	if waitSeconds == 0 {
		waitSeconds = 30
	}
	if waitSeconds < 1 || waitSeconds > 300 {
		return nil, EventsOutput{}, errors.New("wait_seconds must be between 1 and 300")
	}
	// One stream can filter one project or all projects. A fixed multi-project
	// allowlist uses bounded polling so events outside its scope never leak.
	if strings.TrimSpace(input.Project) == "" && h.options.Scope.Mode() == ScopeAllowlist {
		return h.waitPollEvents(ctx, input.Project, input.After, limit, waitSeconds)
	}
	var projectID *int64
	if strings.TrimSpace(input.Project) != "" || h.options.Scope.Mode() == ScopeBound {
		projects, err := h.activityProjects(ctx, input.Project)
		if err != nil {
			return nil, EventsOutput{}, err
		}
		projectID = &projects[0].ID
	}
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds)*time.Second)
	defer cancel()
	response, err := h.options.Client.StreamEventsRaw(waitContext, &generated.StreamEventsRequestOptions{Query: &generated.StreamEventsQuery{AfterID: &input.After, ProjectID: projectID}})
	if err != nil {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), EventsOutput{NextAfterID: input.After, TimedOut: true}, nil
		}
		return nil, EventsOutput{}, err
	}
	defer func() { _ = response.Body.Close() }()
	output, err := readEventStream(waitContext, response.Body, input.After, limit)
	if err != nil {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), EventsOutput{NextAfterID: input.After, TimedOut: true}, nil
		}
		return nil, EventsOutput{}, err
	}
	if err := h.redactEventsOutsideScope(waitContext, &output); err != nil {
		return nil, EventsOutput{}, err
	}
	return successResult(), output, nil
}

func (h toolHandlers) activityProjects(ctx context.Context, name string) ([]ProjectIdentity, error) {
	if strings.TrimSpace(name) != "" || h.options.Scope.Mode() == ScopeBound {
		project, err := h.options.Scope.Project(ctx, h.options.Client, name, false)
		if err != nil {
			return nil, err
		}
		return []ProjectIdentity{project}, nil
	}
	return h.options.Scope.Projects(ctx, h.options.Client, false)
}

func (h toolHandlers) pollScopedEvents(ctx context.Context, projectName string, after, limit int64) (EventsOutput, error) {
	if strings.TrimSpace(projectName) == "" && h.options.Scope.Mode() == ScopeAll {
		response, err := h.options.Client.PollEvents(ctx, &generated.PollEventsRequestOptions{Query: &generated.PollEventsQuery{AfterID: &after, Limit: &limit}})
		if err != nil {
			return EventsOutput{}, err
		}
		return eventsOutput(response), nil
	}
	projects, err := h.activityProjects(ctx, projectName)
	if err != nil {
		return EventsOutput{}, err
	}
	output := EventsOutput{NextAfterID: after}
	for _, project := range projects {
		response, pollErr := h.options.Client.PollProjectEvents(ctx, &generated.PollProjectEventsRequestOptions{PathParams: &generated.PollProjectEventsPath{ProjectID: project.ID}, Query: &generated.PollProjectEventsQuery{AfterID: &after, Limit: &limit}})
		if pollErr != nil {
			return EventsOutput{}, pollErr
		}
		if response.ResetRequired {
			output.ResetRequired = true
			output.ResetAfterID = response.ResetAfterID
			output.NextAfterID = response.NextAfterID
			if output.NextAfterID == 0 && output.ResetAfterID != nil {
				output.NextAfterID = *output.ResetAfterID
			}
			output.Events = nil
			return output, nil
		}
		for _, event := range response.Events {
			output.Events = append(output.Events, streamEvent(event))
		}
	}
	sort.Slice(output.Events, func(i, j int) bool { return output.Events[i].EventID < output.Events[j].EventID })
	if int64(len(output.Events)) > limit {
		output.Events = output.Events[:limit]
	}
	if len(output.Events) > 0 {
		output.NextAfterID = output.Events[len(output.Events)-1].EventID
	}
	if err := h.redactEventsOutsideScope(ctx, &output); err != nil {
		return EventsOutput{}, err
	}
	return output, nil
}

func (h toolHandlers) waitPollEvents(ctx context.Context, project string, after, limit, waitSeconds int64) (*sdkmcp.CallToolResult, EventsOutput, error) {
	waitContext, cancel := context.WithTimeout(ctx, time.Duration(waitSeconds)*time.Second)
	defer cancel()
	for {
		output, err := h.pollScopedEvents(waitContext, project, after, limit)
		if err != nil {
			return nil, EventsOutput{}, err
		}
		if output.ResetRequired || len(output.Events) > 0 {
			return successResult(), output, nil
		}
		select {
		case <-waitContext.Done():
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				return successResult(), EventsOutput{NextAfterID: after, TimedOut: true}, nil
			}
			return nil, EventsOutput{}, waitContext.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func eventsOutput(response *generated.PollEventsBody) EventsOutput {
	if response.ResetRequired {
		return EventsOutput{NextAfterID: response.NextAfterID, ResetRequired: true, ResetAfterID: response.ResetAfterID}
	}
	events := make([]StreamEvent, 0, len(response.Events))
	for _, event := range response.Events {
		events = append(events, streamEvent(event))
	}
	return EventsOutput{Events: events, NextAfterID: response.NextAfterID}
}

func readEventStream(ctx context.Context, reader io.Reader, after, limit int64) (EventsOutput, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), defaultMaxMessageBytes)
	output := EventsOutput{NextAfterID: after}
	eventType := ""
	data := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if eventType == "sync.reset_required" {
				var reset struct {
					ResetAfterID int64 `json:"reset_after_id"`
				}
				if err := json.Unmarshal([]byte(data), &reset); err != nil {
					return EventsOutput{}, fmt.Errorf("decode event reset: %w", err)
				}
				output.ResetRequired = true
				output.ResetAfterID = &reset.ResetAfterID
				output.NextAfterID = reset.ResetAfterID
				output.Events = nil
				return output, nil
			}
			if data != "" && eventType != "" && eventType != "heartbeat" {
				var event generated.EventEnvelope
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					return EventsOutput{}, fmt.Errorf("decode event stream: %w", err)
				}
				output.Events = append(output.Events, streamEvent(event))
				output.NextAfterID = event.EventID
				if int64(len(output.Events)) >= limit {
					return output, nil
				}
				// Return the first available stream batch instead of holding an
				// MCP request open for a future unrelated event.
				return output, nil
			}
			eventType, data = "", ""
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return EventsOutput{}, ctx.Err()
		}
		return EventsOutput{}, err
	}
	return output, nil
}

func streamEvent(event generated.EventEnvelope) StreamEvent {
	output := StreamEvent{EventID: event.EventID, EventUID: event.EventUID, Type: event.Type, Actor: event.Actor, CreatedAt: formatTime(event.CreatedAt), ProjectID: event.ProjectID, ProjectUID: event.ProjectUID, ProjectName: event.ProjectName}
	if event.IssueUID != nil {
		output.IssueUID = *event.IssueUID
	}
	if event.IssueShortID != nil {
		output.IssueShortID = *event.IssueShortID
	}
	if event.RelatedIssueUID != nil {
		output.RelatedIssueUID = *event.RelatedIssueUID
	}
	if event.RelatedIssueShortID != nil {
		output.RelatedIssueShortID = *event.RelatedIssueShortID
	}
	if len(event.Payload) > 0 {
		var payload any
		if json.Unmarshal(event.Payload, &payload) == nil {
			output.Payload = payload
		}
	}
	return output
}

func (h toolHandlers) redactEventsOutsideScope(ctx context.Context, output *EventsOutput) error {
	if output == nil || h.options.Scope.Mode() == ScopeAll || len(output.Events) == 0 {
		return nil
	}
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, false)
	if err != nil {
		return err
	}
	allowedProjects := make(map[int64]struct{}, len(projects))
	allowedProjectUIDs := make(map[string]bool, len(projects))
	for _, project := range projects {
		allowedProjects[project.ID] = struct{}{}
		if project.UID != "" {
			allowedProjectUIDs[project.UID] = true
		}
	}
	// An event's own project passed the scope filter already.
	for _, event := range output.Events {
		if event.ProjectUID != "" {
			allowedProjectUIDs[event.ProjectUID] = true
		}
	}
	peerUIDs := make(map[string]struct{})
	for _, event := range output.Events {
		if event.RelatedIssueUID != "" {
			peerUIDs[event.RelatedIssueUID] = struct{}{}
		}
		collectEventPeerUIDs(event.Payload, peerUIDs)
	}
	allowedPeers := make(map[string]bool, len(peerUIDs))
	for uid := range peerUIDs {
		response, showErr := h.options.Client.ShowIssueByUID(ctx, &generated.ShowIssueByUIDRequestOptions{
			PathParams: &generated.ShowIssueByUIDPath{UID: uid},
		})
		if showErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isHTTPStatus(showErr, http.StatusNotFound) {
				continue
			}
			return errors.New("resolve event peer: daemon request failed")
		}
		_, allowedPeers[uid] = allowedProjects[response.Issue.ProjectID]
	}
	for index := range output.Events {
		event := &output.Events[index]
		if event.RelatedIssueUID == "" || !allowedPeers[event.RelatedIssueUID] {
			event.RelatedIssueUID = ""
			event.RelatedIssueShortID = ""
		}
		event.Payload = redactEventPayloadPeers(event.Payload, allowedPeers, allowedProjectUIDs)
	}
	return nil
}

func isHTTPStatus(err error, status int) bool {
	var apiError *oapiruntime.ClientAPIError
	if errors.As(err, &apiError) && apiError != nil {
		return apiError.StatusCode() == status
	}
	var decodeError *oapiruntime.ResponseDecodeError
	return errors.As(err, &decodeError) && decodeError != nil && decodeError.StatusCode == status
}

var eventPeerUIDKeys = map[string]struct{}{
	"from_uid": {}, "to_uid": {}, "from_issue_uid": {}, "to_issue_uid": {},
	"link_from_uid": {}, "link_to_uid": {}, "peer_uid": {}, "related_issue_uid": {},
	"parent_set_uid": {}, "parent_removed_uid": {},
}

// A display reference survives only when one of its present UID companions
// resolves in scope. Move payloads carry short IDs vouched for by a project
// UID instead of an issue UID, so those keys list a project companion too.
var eventPeerShortIDUIDKeys = map[string][]string{
	"from_short_id":          {"from_uid", "from_issue_uid"},
	"to_short_id":            {"to_uid", "to_issue_uid"},
	"link_from_short_id":     {"link_from_uid"},
	"link_to_short_id":       {"link_to_uid"},
	"peer_short_id":          {"peer_uid"},
	"related_issue_short_id": {"related_issue_uid"},
	"parent_set":             {"parent_set_uid"},
	"parent_removed":         {"parent_removed_uid"},
}

var eventPeerShortIDProjectUIDKeys = map[string]string{
	"from_short_id": "from_project_uid",
	"to_short_id":   "to_project_uid",
}

// Aggregated relationship deltas store parallel UID and display arrays.
var eventPeerUIDListKeys = map[string]string{
	"blocks_added_uids":       "blocks_added",
	"blocks_removed_uids":     "blocks_removed",
	"blocked_by_added_uids":   "blocked_by_added",
	"blocked_by_removed_uids": "blocked_by_removed",
	"related_added_uids":      "related_added",
	"related_removed_uids":    "related_removed",
}

var eventProjectUIDKeys = map[string]struct{}{
	"from_project_uid": {}, "to_project_uid": {}, "source_uid": {},
}

func collectEventPeerUIDs(value any, output map[string]struct{}) {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if _, peerKey := eventPeerUIDKeys[key]; peerKey {
				if uid, ok := nested.(string); ok && uid != "" {
					output[uid] = struct{}{}
				}
			}
			if _, listKey := eventPeerUIDListKeys[key]; listKey {
				if list, ok := nested.([]any); ok {
					for _, element := range list {
						if uid, ok := element.(string); ok && uid != "" {
							output[uid] = struct{}{}
						}
					}
				}
			}
			collectEventPeerUIDs(nested, output)
		}
	case []any:
		for _, nested := range current {
			collectEventPeerUIDs(nested, output)
		}
	}
}

func redactEventPayloadPeers(value any, allowed map[string]bool, allowedProjects map[string]bool) any {
	switch current := value.(type) {
	case map[string]any:
		for key := range current {
			if _, peerKey := eventPeerUIDKeys[key]; peerKey {
				uid, ok := current[key].(string)
				if !ok || !allowed[uid] {
					delete(current, key)
				}
			}
			if _, projectKey := eventProjectUIDKeys[key]; projectKey {
				uid, ok := current[key].(string)
				if !ok || !allowedProjects[uid] {
					delete(current, key)
				}
			}
		}
		for uidsKey, displayKey := range eventPeerUIDListKeys {
			redactEventPeerUIDList(current, uidsKey, displayKey, allowed)
		}
		for shortIDKey, uidKeys := range eventPeerShortIDUIDKeys {
			if _, present := current[shortIDKey]; !present {
				continue
			}
			vouched := false
			for _, uidKey := range uidKeys {
				if uid, ok := current[uidKey].(string); ok && allowed[uid] {
					vouched = true
					break
				}
			}
			if projectKey, paired := eventPeerShortIDProjectUIDKeys[shortIDKey]; paired && !vouched {
				if uid, ok := current[projectKey].(string); ok && allowedProjects[uid] {
					vouched = true
				}
			}
			if !vouched {
				delete(current, shortIDKey)
			}
		}
		for key, nested := range current {
			current[key] = redactEventPayloadPeers(nested, allowed, allowedProjects)
		}
	case []any:
		for index, nested := range current {
			current[index] = redactEventPayloadPeers(nested, allowed, allowedProjects)
		}
	}
	return value
}

func redactEventPeerUIDList(payload map[string]any, uidsKey, displayKey string, allowed map[string]bool) {
	rawUIDs, present := payload[uidsKey]
	rawDisplay, displayPresent := payload[displayKey]
	if !present {
		// A display array with no UID companion cannot be vouched for.
		if displayPresent {
			delete(payload, displayKey)
		}
		return
	}
	uids, ok := rawUIDs.([]any)
	if !ok {
		delete(payload, uidsKey)
		delete(payload, displayKey)
		return
	}
	display, displayOK := rawDisplay.([]any)
	pairwise := displayPresent && displayOK && len(display) == len(uids)
	keptUIDs := make([]any, 0, len(uids))
	keptDisplay := make([]any, 0, len(uids))
	for index, element := range uids {
		uid, isString := element.(string)
		if !isString || !allowed[uid] {
			continue
		}
		keptUIDs = append(keptUIDs, uid)
		if pairwise {
			keptDisplay = append(keptDisplay, display[index])
		}
	}
	if len(keptUIDs) == 0 {
		delete(payload, uidsKey)
		delete(payload, displayKey)
		return
	}
	payload[uidsKey] = keptUIDs
	if displayPresent {
		if pairwise {
			payload[displayKey] = keptDisplay
		} else {
			delete(payload, displayKey)
		}
	}
}
