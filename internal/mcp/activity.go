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
	"strconv"
	"strings"
	"time"

	oapiruntime "github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kata/internal/shortid"
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
	Limit      int64  `json:"limit,omitempty" jsonschema:"Maximum rows from 1 through 100; default 20"`
	Cursor     string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor from the previous result's next_cursor; keep the other filters identical and omit for the first page"`
}

// AuditClosesOutput contains close audit records for one project.
type AuditClosesOutput struct {
	Project    ProjectIdentity           `json:"project"`
	Rows       []generated.AuditCloseRow `json:"rows"`
	Truncated  bool                      `json:"truncated,omitempty"`
	NextCursor *string                   `json:"next_cursor,omitempty"`
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
	project, target, qualified, err := h.destructiveTarget(ctx, input.Ref, input.Confirm, "DELETE")
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.DeleteIssue(ctx, &generated.DeleteIssueRequestOptions{
		PathParams: &generated.DeleteIssuePath{ProjectID: project.ID, Ref: target.UID},
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
	project, target, qualified, err := h.destructiveTarget(ctx, input.Ref, input.Confirm, "PURGE")
	if err != nil {
		return nil, PurgeOutput{}, err
	}
	response, err := h.options.Client.PurgeIssue(ctx, &generated.PurgeIssueRequestOptions{
		PathParams: &generated.PurgeIssuePath{ProjectID: project.ID, Ref: target.UID},
		Body:       &generated.PurgeIssueBody{Actor: h.options.Actor, Reason: optionalString(input.Reason)},
		Header:     &generated.PurgeIssueHeaders{XKataConfirm: &qualified},
	})
	if err != nil {
		return nil, PurgeOutput{}, err
	}
	return successResult(), PurgeOutput{Project: project, Ref: project.Name + "#" + target.ShortID, Purged: true, Log: response.PurgeLog}, nil
}

// destructiveIssue is the issue confirmed by a destructive preflight. UID
// addresses the mutation so a short ID reused after the preflight cannot
// redirect it; ShortID is the confirmed display reference.
type destructiveIssue struct {
	UID     string
	ShortID string
}

func (h toolHandlers) destructiveTarget(ctx context.Context, rawRef, confirm, verb string) (ProjectIdentity, destructiveIssue, string, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, rawRef, true)
	if err != nil {
		return ProjectIdentity{}, destructiveIssue{}, "", err
	}
	// Resolve soft-deleted rows for DELETE as well as PURGE so a retry
	// after a lost response reaches the daemon's idempotent re-delete
	// instead of failing this preflight with "not found".
	includeDeleted := true
	shown, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
	})
	if err != nil {
		return ProjectIdentity{}, destructiveIssue{}, "", err
	}
	canonical := project.Name + "#" + shown.Issue.ShortID
	expected := verb + " " + canonical
	if confirm != expected {
		return ProjectIdentity{}, destructiveIssue{}, "", fmt.Errorf("confirm must equal %q", expected)
	}
	return project, destructiveIssue{UID: shown.Issue.UID, ShortID: shown.Issue.ShortID}, expected, nil
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
	h = h.withLongRunningClient()
	// The deadline can expire while a lookup is in flight; that is the
	// documented timeout outcome, not a tool error. States from the last
	// complete round keep the output honest.
	lastStates := make([]WaitState, 0)
	for {
		states := make([]WaitState, 0, len(input.Refs))
		matches := 0
		for _, raw := range input.Refs {
			project, ref, targetErr := h.options.Scope.IssueTarget(waitContext, h.options.Client, raw, false)
			if targetErr != nil {
				if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
					return successResult(), WaitOutput{Reason: "timeout", States: lastStates}, nil
				}
				return nil, WaitOutput{}, targetErr
			}
			includeDeleted := true
			shown, showErr := h.options.Client.ShowIssue(waitContext, &generated.ShowIssueRequestOptions{
				PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
				Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
			})
			if showErr != nil {
				if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
					return successResult(), WaitOutput{Reason: "timeout", States: lastStates}, nil
				}
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
		lastStates = states
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
	if err := h.validateAuditParentFilter(ctx, project, input.Parent); err != nil {
		return nil, AuditClosesOutput{}, err
	}
	limit := input.Limit
	if limit == 0 {
		limit = defaultResultLimit
	}
	if limit < 1 || limit > maximumResultLimit {
		return nil, AuditClosesOutput{}, fmt.Errorf("limit must be between 1 and %d", maximumResultLimit)
	}
	response, err := h.options.Client.AuditCloses(ctx, &generated.AuditClosesRequestOptions{Query: &generated.AuditClosesQuery{
		ProjectID: project.ID, Since: optionalString(input.Since), Until: optionalString(input.Until),
		Actor: optionalString(input.Actor), Parent: optionalString(input.Parent), Reason: optionalString(input.Reason),
		NoEvidence: optionalTrue(input.NoEvidence),
	}})
	if err != nil {
		return nil, AuditClosesOutput{}, err
	}
	// Rows are ordered by immutable close-event ID. The cursor records the
	// last returned event ID plus the number of rows at or below it, so a
	// below-cursor change — history merged in from another project or rows
	// removed by a purge — invalidates pagination explicitly instead of
	// silently skipping or repeating rows.
	rows := response.Rows
	start := 0
	if input.Cursor != "" {
		cursorEventID, seen, cursorErr := parseAuditCursor(input.Cursor)
		if cursorErr != nil {
			return nil, AuditClosesOutput{}, cursorErr
		}
		for start < len(rows) && rows[start].EventID <= cursorEventID {
			start++
		}
		if int64(start) != seen || start == 0 || rows[start-1].EventID != cursorEventID {
			return nil, AuditClosesOutput{}, errors.New("close history below the pagination cursor changed (project merge or issue purge); restart without a cursor")
		}
	}
	rows = rows[start:]
	output := AuditClosesOutput{Project: project}
	if int64(len(rows)) > limit {
		rows = rows[:limit]
		output.Truncated = true
		next := encodeAuditCursor(rows[len(rows)-1].EventID, int64(start+len(rows)))
		output.NextCursor = &next
	}
	rows, err = h.redactAuditParentsOutsideScope(ctx, project, rows)
	if err != nil {
		return nil, AuditClosesOutput{}, err
	}
	output.Rows = rows
	return successResult(), output, nil
}

func encodeAuditCursor(eventID, seen int64) string {
	return fmt.Sprintf("v1:%d:%d", eventID, seen)
}

func parseAuditCursor(raw string) (eventID, seen int64, err error) {
	var version string
	parts := strings.Split(raw, ":")
	if len(parts) == 3 {
		version = parts[0]
		eventID, err = strconv.ParseInt(parts[1], 10, 64)
		if err == nil {
			seen, err = strconv.ParseInt(parts[2], 10, 64)
		}
	}
	if version != "v1" || err != nil || eventID < 1 || seen < 1 {
		return 0, 0, errors.New("cursor is not a value returned in next_cursor")
	}
	return eventID, seen, nil
}

// validateAuditParentFilter stops fixed scopes from probing foreign refs
// through the daemon's global parent-filter resolution.
func (h toolHandlers) validateAuditParentFilter(ctx context.Context, project ProjectIdentity, raw string) error {
	ref := strings.TrimSpace(raw)
	if ref == "" || h.options.Scope.Mode() == ScopeAll {
		return nil
	}
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return fmt.Errorf("parent filter %q is invalid", ref)
	}
	if parsed.ULID != "" {
		return fmt.Errorf("parent filter %q is an unscoped UID; use a project-qualified short reference", ref)
	}
	if len(parsed.ShortID) == shortid.MaxLength {
		return fmt.Errorf("parent filter %q uses an ambiguous full-length short ID; use a shorter project-qualified reference", ref)
	}
	if parsed.Project == "" {
		// The daemon matches unresolved bare filters against stored parent
		// snapshots, which can name purged foreign parents. Forward a bare
		// filter only when it resolves in the audited project; otherwise
		// fail closed instead of exposing a snapshot-matching oracle.
		found, probeErr := h.issueResolvesInProject(ctx, project, parsed.ShortID)
		if probeErr != nil {
			return probeErr
		}
		if !found {
			return fmt.Errorf("parent filter %q does not resolve in project %q; use a project-qualified reference to an in-scope parent", ref, project.Name)
		}
		return nil
	}
	qualifiedProject, err := h.options.Scope.Project(ctx, h.options.Client, parsed.Project, true)
	if err != nil {
		return err
	}
	found, err := h.issueResolvesInProject(ctx, qualifiedProject, parsed.ShortID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("parent filter %q does not resolve in project %q", ref, qualifiedProject.Name)
	}
	return nil
}

// redactAuditParentsOutsideScope authorizes parent display refs by the
// close-time frozen parent UID: a short ID is project-local and mutable, so a
// purged foreign parent can collide with an unrelated in-scope short ID.
// Rows whose frozen parent no longer resolves in scope, and legacy rows with
// no frozen identity behind a bare ref, fail closed. Qualified refs without a
// UID keep the current-name check because the daemon renders them from
// currently resolved projects.
func (h toolHandlers) redactAuditParentsOutsideScope(ctx context.Context, _ ProjectIdentity, rows []generated.AuditCloseRow) ([]generated.AuditCloseRow, error) {
	if h.options.Scope.Mode() == ScopeAll {
		return rows, nil
	}
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, true)
	if err != nil {
		return nil, err
	}
	inScopeNames := make(map[string]struct{}, len(projects))
	inScopeIDs := make(map[int64]struct{}, len(projects))
	for _, scoped := range projects {
		inScopeNames[scoped.Name] = struct{}{}
		inScopeIDs[scoped.ID] = struct{}{}
	}
	resolved := make(map[string]bool)
	blank := func(index int) {
		rows[index].Parent = nil
		rows[index].ParentUID = nil
	}
	for index := range rows {
		if rows[index].Parent == nil {
			rows[index].ParentUID = nil
			continue
		}
		if rows[index].ParentUID != nil && *rows[index].ParentUID != "" {
			uid := *rows[index].ParentUID
			allowed, seen := resolved[uid]
			if !seen {
				allowed, err = h.issueUIDInScope(ctx, uid, inScopeIDs)
				if err != nil {
					return nil, err
				}
				resolved[uid] = allowed
			}
			if !allowed {
				blank(index)
			}
			continue
		}
		qualifier, _, qualified := strings.Cut(*rows[index].Parent, "#")
		if qualified {
			if _, ok := inScopeNames[qualifier]; !ok {
				blank(index)
			}
			continue
		}
		blank(index)
	}
	return rows, nil
}

// issueUIDInScope reports whether an issue UID currently resolves inside the
// startup scope. A transient daemon failure fails closed with a redacted
// error; a purged UID simply does not resolve.
func (h toolHandlers) issueUIDInScope(ctx context.Context, uid string, allowedProjects map[int64]struct{}) (bool, error) {
	response, err := h.options.Client.ShowIssueByUID(ctx, &generated.ShowIssueByUIDRequestOptions{
		PathParams: &generated.ShowIssueByUIDPath{UID: uid},
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if isHTTPStatus(err, http.StatusNotFound) {
			return false, nil
		}
		return false, errors.New("resolve scoped issue reference: daemon request failed")
	}
	_, ok := allowedProjects[response.Issue.ProjectID]
	return ok, nil
}

// issueResolvesInProject reports whether ref resolves in project, including
// soft-deleted issues. A transient daemon failure fails closed.
func (h toolHandlers) issueResolvesInProject(ctx context.Context, project ProjectIdentity, ref string) (bool, error) {
	includeDeleted := true
	_, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
		Query:      &generated.ShowIssueQuery{IncludeDeleted: &includeDeleted},
	})
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if isHTTPStatus(err, http.StatusNotFound) {
		return false, nil
	}
	return false, errors.New("resolve scoped issue reference: daemon request failed")
}

func (h toolHandlers) digest(ctx context.Context, _ *sdkmcp.CallToolRequest, input DigestInput) (*sdkmcp.CallToolResult, DigestOutput, error) {
	if strings.TrimSpace(input.Since) == "" {
		return nil, DigestOutput{}, errors.New("since is required")
	}
	aggregate := strings.TrimSpace(input.Project) == "" && h.options.Scope.Mode() != ScopeBound
	projects, err := h.activityProjects(ctx, input.Project)
	if err != nil {
		return nil, DigestOutput{}, err
	}
	out := make([]ProjectDigest, 0, len(projects))
	for _, project := range projects {
		response, digestErr := h.options.Client.DigestProject(ctx, &generated.DigestProjectRequestOptions{PathParams: &generated.DigestProjectPath{ProjectID: project.ID}, Query: &generated.DigestProjectQuery{Since: input.Since, Until: optionalString(input.Until), Actor: input.Actors}})
		if digestErr != nil {
			if aggregate && isHTTPStatus(digestErr, http.StatusNotFound) {
				continue
			}
			return nil, DigestOutput{}, digestErr
		}
		if h.options.Scope.Mode() != ScopeAll {
			h.redactDigestLinkActions(response)
		}
		projectCopy := project
		out = append(out, ProjectDigest{Project: &projectCopy, Digest: *response})
	}
	return successResult(), DigestOutput{Digests: out}, nil
}

// Digest action tokens that carry a peer issue short ID after the colon.
var digestLinkActionPrefixes = map[string]struct{}{
	"blocks": {}, "blocked_by": {}, "parent": {}, "related": {},
	"unblocks": {}, "unblocked_by": {}, "unparent": {}, "unrelated": {},
}

// redactDigestLinkActions strips the peer short ID from link action tokens
// on scoped servers. Tokens carry only project-local short IDs from
// historical events, and links change over time, so no current-state lookup
// can prove a target's identity: a historical foreign peer can collide with
// a current in-scope short ID. The action type survives; the target does not.
func (h toolHandlers) redactDigestLinkActions(digest *generated.DigestResponseBody) {
	for actorIndex := range digest.Actors {
		for issueIndex := range digest.Actors[actorIndex].Issues {
			issueActions := &digest.Actors[actorIndex].Issues[issueIndex]
			kept := make([]string, 0, len(issueActions.Actions))
			for _, action := range issueActions.Actions {
				prefix, target, split := strings.Cut(action, ":")
				if !split || target == "" {
					kept = append(kept, action)
					continue
				}
				if _, linkAction := digestLinkActionPrefixes[prefix]; !linkAction {
					kept = append(kept, action)
					continue
				}
				kept = append(kept, prefix)
			}
			issueActions.Actions = kept
		}
	}
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
		return successResult(), withEventsList(output), nil
	}
	waitSeconds := input.WaitSeconds
	if waitSeconds == 0 {
		waitSeconds = 30
	}
	if waitSeconds < 1 || waitSeconds > 300 {
		return nil, EventsOutput{}, errors.New("wait_seconds must be between 1 and 300")
	}
	h = h.withLongRunningClient()
	// One stream can safely filter one active project. Dynamic and fixed
	// multi-project scopes use bounded per-project polling so archived projects
	// cannot leak through the daemon-global stream.
	if strings.TrimSpace(input.Project) == "" && h.options.Scope.Mode() != ScopeBound {
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
			return successResult(), withEventsList(EventsOutput{NextAfterID: input.After, TimedOut: true}), nil
		}
		return nil, EventsOutput{}, err
	}
	defer func() { _ = response.Body.Close() }()
	output, err := readEventStream(waitContext, response.Body, input.After, limit)
	if err != nil {
		if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
			return successResult(), withEventsList(EventsOutput{NextAfterID: input.After, TimedOut: true}), nil
		}
		return nil, EventsOutput{}, err
	}
	if err := h.redactEventsOutsideScope(waitContext, &output); err != nil {
		return nil, EventsOutput{}, err
	}
	return successResult(), withEventsList(output), nil
}

// withEventsList upholds the required-collection contract: successful
// responses, including empty polls, timeouts, and resets, serialize events
// as an empty array rather than null.
func withEventsList(output EventsOutput) EventsOutput {
	if output.Events == nil {
		output.Events = []StreamEvent{}
	}
	return output
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
	aggregate := strings.TrimSpace(projectName) == "" && h.options.Scope.Mode() != ScopeBound
	projects, err := h.activityProjects(ctx, projectName)
	if err != nil {
		return EventsOutput{}, err
	}
	output := EventsOutput{NextAfterID: after}
	for _, project := range projects {
		response, pollErr := h.options.Client.PollProjectEvents(ctx, &generated.PollProjectEventsRequestOptions{PathParams: &generated.PollProjectEventsPath{ProjectID: project.ID}, Query: &generated.PollProjectEventsQuery{AfterID: &after, Limit: &limit}})
		if pollErr != nil {
			if aggregate && isHTTPStatus(pollErr, http.StatusNotFound) {
				continue
			}
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
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				return successResult(), withEventsList(EventsOutput{NextAfterID: after, TimedOut: true}), nil
			}
			return nil, EventsOutput{}, err
		}
		if output.ResetRequired || len(output.Events) > 0 {
			return successResult(), withEventsList(output), nil
		}
		select {
		case <-waitContext.Done():
			if errors.Is(waitContext.Err(), context.DeadlineExceeded) {
				return successResult(), withEventsList(EventsOutput{NextAfterID: after, TimedOut: true}), nil
			}
			return nil, EventsOutput{}, waitContext.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
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
	// An event's own immutable project UID passed the scope filter already.
	// Names are NOT pooled across the batch: a historical name stamped on an
	// old event can currently belong to an out-of-scope project.
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
		redactEventTypedDisplayRefs(event)
	}
	return nil
}

// redactEventTypedDisplayRefs removes event-type-specific display references
// that carry no UID companion: close-throttle cohort refs span projects and
// close-evidence refs are stored verbatim. Historical text refs are
// authorized only against this event's own stamped project name — current
// scope names are unusable because a scoped project renamed to a previously
// foreign name would retroactively vouch that foreign project's refs.
func redactEventTypedDisplayRefs(event *StreamEvent) {
	eventProjectName := event.ProjectName
	payload, ok := event.Payload.(map[string]any)
	if !ok {
		return
	}
	switch event.Type {
	case "close.throttled":
		for _, key := range []string{"parent", "prior"} {
			if ref, isString := payload[key].(string); isString && ref != "" && !displayRefInScope(ref, eventProjectName) {
				delete(payload, key)
			}
		}
		cohort, isList := payload["cohort"].([]any)
		if !isList {
			return
		}
		kept := make([]any, 0, len(cohort))
		for _, element := range cohort {
			ref, isString := element.(string)
			if isString && displayRefInScope(ref, eventProjectName) {
				kept = append(kept, ref)
			}
		}
		if len(kept) == 0 {
			delete(payload, "cohort")
		} else {
			payload["cohort"] = kept
		}
	case "issue.closed":
		evidence, isList := payload["evidence"].([]any)
		if !isList {
			return
		}
		for _, element := range evidence {
			entry, isMap := element.(map[string]any)
			if !isMap {
				continue
			}
			if ref, isString := entry["issue_ref"].(string); isString && ref != "" && !displayRefInScope(ref, eventProjectName) {
				delete(entry, "issue_ref")
			}
		}
	}
}

// displayRefInScope applies the display-reference rendering contract:
// bare refs are same-project and full-length values are globally resolved
// and unprovable. A qualified ref survives only when it names the event's
// own stamped project — the one identity that provably matched an in-scope
// project when the text was written.
func displayRefInScope(ref, eventProjectName string) bool {
	qualifier, _, qualified := strings.Cut(ref, "#")
	if qualified {
		return eventProjectName != "" && qualifier == eventProjectName
	}
	return len(ref) < shortid.MaxLength
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
	"parent_uid": {}, "parent_set_uid": {}, "parent_removed_uid": {},
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
	"parent_short_id":        {"parent_uid"},
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

// Relationship references appear only in the top-level payload map and in
// the objects of a top-level "links" array (issue.created / issue.snapshot).
// Nested subtrees such as "metadata" and "diff" carry opaque user keys and
// values, so they are never inspected: a user metadata key named source_uid
// or from_uid is data, not a relationship reference.
func forEachPeerBearingMap(payload any, visit func(map[string]any)) {
	root, ok := payload.(map[string]any)
	if !ok {
		return
	}
	visit(root)
	links, ok := root["links"].([]any)
	if !ok {
		return
	}
	for _, element := range links {
		if link, isMap := element.(map[string]any); isMap {
			visit(link)
		}
	}
}

func collectEventPeerUIDs(payload any, output map[string]struct{}) {
	forEachPeerBearingMap(payload, func(current map[string]any) {
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
		}
	})
}

func redactEventPayloadPeers(payload any, allowed map[string]bool, allowedProjects map[string]bool) any {
	forEachPeerBearingMap(payload, func(current map[string]any) {
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
	})
	return payload
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
