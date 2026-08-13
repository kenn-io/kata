package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/shortid"
	"go.kenn.io/kata/pkg/client/generated"
)

const (
	defaultResultLimit  = 20
	maximumResultLimit  = 100
	defaultCommentLimit = 20
)

type toolHandlers struct {
	options Options
}

func (h toolHandlers) search(ctx context.Context, _ *sdkmcp.CallToolRequest, input SearchInput) (*sdkmcp.CallToolResult, SearchOutput, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return nil, SearchOutput{}, errors.New("query must not be empty")
	}
	limit, err := boundedLimit(input.Limit)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	var mode *generated.SearchIssuesQueryMode
	if input.Mode != "" {
		value := generated.SearchIssuesQueryMode(input.Mode)
		if err := value.Validate(); err != nil {
			return nil, SearchOutput{}, fmt.Errorf("mode: %w", err)
		}
		mode = &value
	}
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, SearchOutput{}, err
	}
	type projectSearch struct {
		response *generated.SearchIssuesResponse
		err      error
	}
	searches := make([]projectSearch, len(projects))
	group, groupContext := errgroup.WithContext(ctx)
	group.SetLimit(toolCallConcurrency)
	for index := range projects {
		index := index
		group.Go(func() error {
			limit64 := int64(limit + 1)
			response, searchErr := h.options.Client.SearchIssues(groupContext, &generated.SearchIssuesRequestOptions{
				PathParams: &generated.SearchIssuesPath{ProjectID: projects[index].ID},
				Query: &generated.SearchIssuesQuery{
					Q:            query,
					Limit:        &limit64,
					Mode:         mode,
					Label:        compactStrings(input.Labels),
					ExcludeLabel: compactStrings(input.ExcludeLabels),
				},
			})
			searches[index] = projectSearch{response: response, err: searchErr}
			if searchErr != nil {
				return fmt.Errorf("search project %q: %w", projects[index].Name, searchErr)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, SearchOutput{}, err
	}
	results := make([]SearchHit, 0)
	truncated := false
	degraded := false
	degradedReasons := make([]string, 0)
	effectiveMode := ""
	for index, search := range searches {
		response := search.response
		if effectiveMode == "" {
			effectiveMode = response.Mode
		}
		if len(response.Results) > limit {
			truncated = true
		}
		for _, hit := range response.Results {
			results = append(results, SearchHit{
				Issue:     h.summaryFromIssue(projects[index], hit.Issue),
				Score:     hit.Score,
				MatchedIn: nonNilStrings(hit.MatchedIn),
			})
		}
		if response.Degraded != nil && *response.Degraded {
			degraded = true
		}
		if response.DegradedReason != nil && *response.DegradedReason != "" {
			degradedReasons = append(degradedReasons, projects[index].Name+": "+*response.DegradedReason)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Issue.QualifiedRef < results[j].Issue.QualifiedRef
	})
	if len(results) > limit {
		results = results[:limit]
		truncated = true
	}
	project, outputProjects := outputProjectScope(projects)
	return successResult(), SearchOutput{
		Project:        project,
		Projects:       outputProjects,
		Query:          query,
		Mode:           effectiveMode,
		Degraded:       degraded,
		DegradedReason: strings.Join(degradedReasons, "; "),
		Results:        results,
		Truncated:      truncated,
	}, nil
}

func (h toolHandlers) list(ctx context.Context, _ *sdkmcp.CallToolRequest, input ListInput) (*sdkmcp.CallToolResult, IssueListOutput, error) {
	limit, err := boundedLimit(input.Limit)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	if input.Unowned && strings.TrimSpace(input.Owner) != "" {
		return nil, IssueListOutput{}, errors.New("owner and unowned are mutually exclusive")
	}
	var status *generated.ListIssuesQueryStatus
	var globalStatus *generated.ListAllIssuesQueryStatus
	if input.Status != "" {
		value := generated.ListIssuesQueryStatus(input.Status)
		if err := value.Validate(); err != nil {
			return nil, IssueListOutput{}, fmt.Errorf("status: %w", err)
		}
		status = &value
		globalValue := generated.ListAllIssuesQueryStatus(input.Status)
		globalStatus = &globalValue
	}
	priority, err := priorityQuery(input.Priority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	maxPriority, err := priorityQuery(input.MaxPriority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	limit64 := int64(limit + 1)
	owner := optionalString(input.Owner)
	unowned := optionalTrue(input.Unowned)
	issues := make([]IssueSummary, 0)
	truncated := false
	if h.options.Scope.Mode() == ScopeAllowlist && strings.TrimSpace(input.Project) == "" {
		for _, project := range projects {
			response, listErr := h.options.Client.ListIssues(ctx, &generated.ListIssuesRequestOptions{
				PathParams: &generated.ListIssuesPath{ProjectID: project.ID},
				Query: &generated.ListIssuesQuery{
					Status: status, Priority: priority, MaxPriority: maxPriority, Limit: &limit64,
					Unowned: unowned, Owner: owner, Label: compactStrings(input.Labels),
					ExcludeLabel: compactStrings(input.ExcludeLabels), Meta: compactStrings(input.Metadata),
				},
			})
			if listErr != nil {
				return nil, IssueListOutput{}, listErr
			}
			if len(response.Issues) > limit {
				truncated = true
			}
			for _, issue := range response.Issues {
				issues = append(issues, h.summaryFromIssueOut(project, issue))
			}
		}
		sortIssueSummaries(issues)
		truncated = truncated || len(issues) > limit
	} else if len(projects) == 1 && (h.options.Scope.Mode() == ScopeBound || strings.TrimSpace(input.Project) != "") {
		response, listErr := h.options.Client.ListIssues(ctx, &generated.ListIssuesRequestOptions{
			PathParams: &generated.ListIssuesPath{ProjectID: projects[0].ID},
			Query: &generated.ListIssuesQuery{
				Status: status, Priority: priority, MaxPriority: maxPriority, Limit: &limit64,
				Unowned: unowned, Owner: owner, Label: compactStrings(input.Labels),
				ExcludeLabel: compactStrings(input.ExcludeLabels), Meta: compactStrings(input.Metadata),
			},
		})
		if listErr != nil {
			return nil, IssueListOutput{}, listErr
		}
		truncated = len(response.Issues) > limit
		for _, issue := range response.Issues {
			issues = append(issues, h.summaryFromIssueOut(projects[0], issue))
		}
	} else {
		response, listErr := h.options.Client.ListAllIssues(ctx, &generated.ListAllIssuesRequestOptions{
			Query: &generated.ListAllIssuesQuery{
				Status: globalStatus, Priority: priority, MaxPriority: maxPriority, Limit: &limit64,
				Unowned: unowned, Owner: owner, Label: compactStrings(input.Labels),
				ExcludeLabel: compactStrings(input.ExcludeLabels), Meta: compactStrings(input.Metadata),
			},
		})
		if listErr != nil {
			return nil, IssueListOutput{}, listErr
		}
		allowed := projectIDSet(projects)
		for _, issue := range response.Issues {
			if _, ok := allowed[issue.ProjectID]; ok {
				issues = append(issues, summaryFromGlobalIssue(issue))
			}
		}
		truncated = len(response.Issues) > limit || len(issues) > limit
	}
	if len(issues) > limit {
		issues = issues[:limit]
	}
	project, outputProjects := outputProjectScope(projects)
	return successResult(), IssueListOutput{
		Project:   project,
		Projects:  outputProjects,
		Issues:    issues,
		Truncated: truncated,
	}, nil
}

func (h toolHandlers) ready(ctx context.Context, _ *sdkmcp.CallToolRequest, input ReadyInput) (*sdkmcp.CallToolResult, IssueListOutput, error) {
	limit, err := boundedLimit(input.Limit)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	if input.Unowned && strings.TrimSpace(input.Owner) != "" {
		return nil, IssueListOutput{}, errors.New("owner and unowned are mutually exclusive")
	}
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	limit64 := int64(limit + 1)
	issues := make([]IssueSummary, 0)
	truncated := false
	if h.options.Scope.Mode() == ScopeAllowlist && strings.TrimSpace(input.Project) == "" {
		for _, project := range projects {
			response, readyErr := h.options.Client.ReadyIssues(ctx, &generated.ReadyIssuesRequestOptions{
				PathParams: &generated.ReadyIssuesPath{ProjectID: project.ID},
				Query: &generated.ReadyIssuesQuery{
					Limit: &limit64, Unowned: optionalTrue(input.Unowned), Owner: optionalString(input.Owner),
					Label: compactStrings(input.Labels), ExcludeLabel: compactStrings(input.ExcludeLabels),
				},
			})
			if readyErr != nil {
				return nil, IssueListOutput{}, readyErr
			}
			if len(response.Issues) > limit {
				truncated = true
			}
			for _, issue := range response.Issues {
				issues = append(issues, h.summaryFromIssueOut(project, issue))
			}
		}
		sortIssueSummaries(issues)
		truncated = truncated || len(issues) > limit
	} else if len(projects) == 1 && (h.options.Scope.Mode() == ScopeBound || strings.TrimSpace(input.Project) != "") {
		response, readyErr := h.options.Client.ReadyIssues(ctx, &generated.ReadyIssuesRequestOptions{
			PathParams: &generated.ReadyIssuesPath{ProjectID: projects[0].ID},
			Query: &generated.ReadyIssuesQuery{
				Limit: &limit64, Unowned: optionalTrue(input.Unowned), Owner: optionalString(input.Owner),
				Label: compactStrings(input.Labels), ExcludeLabel: compactStrings(input.ExcludeLabels),
			},
		})
		if readyErr != nil {
			return nil, IssueListOutput{}, readyErr
		}
		truncated = len(response.Issues) > limit
		for _, issue := range response.Issues {
			issues = append(issues, h.summaryFromIssueOut(projects[0], issue))
		}
	} else {
		response, readyErr := h.options.Client.ReadyIssuesGlobal(ctx, &generated.ReadyIssuesGlobalRequestOptions{
			Query: &generated.ReadyIssuesGlobalQuery{
				Limit: &limit64, Unowned: optionalTrue(input.Unowned), Owner: optionalString(input.Owner),
				Label: compactStrings(input.Labels), ExcludeLabel: compactStrings(input.ExcludeLabels),
			},
		})
		if readyErr != nil {
			return nil, IssueListOutput{}, readyErr
		}
		allowed := projectIDSet(projects)
		for _, issue := range response.Issues {
			if _, ok := allowed[issue.ProjectID]; ok {
				issues = append(issues, summaryFromReadyGlobalIssue(issue))
			}
		}
		truncated = len(response.Issues) > limit || len(issues) > limit
	}
	if len(issues) > limit {
		issues = issues[:limit]
	}
	project, outputProjects := outputProjectScope(projects)
	return successResult(), IssueListOutput{
		Project:   project,
		Projects:  outputProjects,
		Issues:    issues,
		Truncated: truncated,
	}, nil
}

func sortIssueSummaries(issues []IssueSummary) {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].UpdatedAt != issues[j].UpdatedAt {
			return issues[i].UpdatedAt > issues[j].UpdatedAt
		}
		return issues[i].QualifiedRef < issues[j].QualifiedRef
	})
}

func (h toolHandlers) labels(ctx context.Context, _ *sdkmcp.CallToolRequest, input LabelsInput) (*sdkmcp.CallToolResult, LabelsOutput, error) {
	projects, err := h.readProjects(ctx, input.Project)
	if err != nil {
		return nil, LabelsOutput{}, err
	}
	counts := make(map[string]int64)
	for _, project := range projects {
		response, listErr := h.options.Client.ListLabels(ctx, &generated.ListLabelsRequestOptions{
			PathParams: &generated.ListLabelsPath{ProjectID: project.ID},
		})
		if listErr != nil {
			return nil, LabelsOutput{}, fmt.Errorf("list labels for project %q: %w", project.Name, listErr)
		}
		for _, label := range response.Labels {
			counts[label.Label] += label.Count
		}
	}
	labels := make([]LabelCount, 0, len(counts))
	for label, count := range counts {
		labels = append(labels, LabelCount{Label: label, Count: count})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].Label < labels[j].Label })
	project, outputProjects := outputProjectScope(projects)
	return successResult(), LabelsOutput{Project: project, Projects: outputProjects, Labels: labels}, nil
}

func (h toolHandlers) show(ctx context.Context, _ *sdkmcp.CallToolRequest, input ShowInput) (*sdkmcp.CallToolResult, ShowOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, false)
	if err != nil {
		return nil, ShowOutput{}, err
	}
	commentLimit := input.CommentLimit
	if commentLimit == 0 {
		commentLimit = defaultCommentLimit
	}
	if commentLimit < 0 || commentLimit > maximumResultLimit {
		return nil, ShowOutput{}, fmt.Errorf("comment_limit must be between 1 and %d when set", maximumResultLimit)
	}
	response, err := h.options.Client.ShowIssue(ctx, &generated.ShowIssueRequestOptions{
		PathParams: &generated.ShowIssuePath{ProjectID: project.ID, Ref: ref},
	})
	if err != nil {
		return nil, ShowOutput{}, err
	}
	comments := response.Comments
	truncated := len(comments) > commentLimit
	if truncated {
		comments = comments[len(comments)-commentLimit:]
	}
	commentSummaries := make([]CommentSummary, 0, len(comments))
	for _, comment := range comments {
		commentSummaries = append(commentSummaries, commentSummary(comment))
	}
	labels := make([]string, 0, len(response.Labels))
	for _, label := range response.Labels {
		labels = append(labels, label.Label)
	}
	links := make([]LinkSummary, 0, len(response.Links))
	var allowedLinkProjects map[string]struct{}
	if h.options.Scope.Mode() != ScopeAll {
		projects, scopeErr := h.options.Scope.Projects(ctx, h.options.Client, false)
		if scopeErr != nil {
			return nil, ShowOutput{}, scopeErr
		}
		allowedLinkProjects = make(map[string]struct{}, len(projects))
		for _, allowedProject := range projects {
			allowedLinkProjects[allowedProject.Name] = struct{}{}
		}
	}
	for _, link := range response.Links {
		peer, _ := linkPeerAndType(response.Issue.UID, link)
		if allowedLinkProjects != nil {
			if _, allowed := allowedLinkProjects[peer.Project]; !allowed {
				continue
			}
		}
		links = append(links, linkSummary(response.Issue.UID, link))
	}
	summary := h.summaryFromIssue(project, response.Issue)
	metadata := response.Issue.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return successResult(), ShowOutput{
		Project: project,
		Issue: IssueDetail{
			IssueSummary: summary,
			Labels:       labels,
			Body:         response.Issue.Body,
			Author:       response.Issue.Author,
			Metadata:     metadata,
			Links:        links,
			Comments:     commentSummaries,
		},
		CommentsTruncated: truncated,
	}, nil
}

func (h toolHandlers) create(ctx context.Context, _ *sdkmcp.CallToolRequest, input CreateInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return nil, MutationOutput{}, errors.New("title must not be empty")
	}
	key := strings.TrimSpace(input.IdempotencyKey)
	if key == "" {
		return nil, MutationOutput{}, errors.New("idempotency_key must not be empty")
	}
	if err := validatePriority(input.Priority); err != nil {
		return nil, MutationOutput{}, err
	}
	project, err := h.createTarget(ctx, input.Project)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	links, err := h.createLinks(ctx, project, input)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	issueMetadata, err := createMetadata(input)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	body := generated.CreateIssueBody{
		Actor:    &h.options.Actor,
		Title:    title,
		Labels:   compactStrings(input.Labels),
		Links:    links,
		Metadata: issueMetadata,
		Priority: input.Priority,
		ForceNew: optionalTrue(input.ForceNew),
	}
	if input.Body != "" {
		body.Body = &input.Body
	}
	if strings.TrimSpace(input.Owner) != "" {
		body.Owner = &input.Owner
	}
	response, err := h.options.Client.CreateIssue(ctx, &generated.CreateIssueRequestOptions{
		PathParams: &generated.CreateIssuePath{ProjectID: project.ID},
		Body:       &body,
		Header:     &generated.CreateIssueHeaders{IdempotencyKey: &key},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) edit(ctx context.Context, _ *sdkmcp.CallToolRequest, input EditInput) (*sdkmcp.CallToolResult, EditOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, EditOutput{}, err
	}
	if input.ClearOwner && input.Owner != nil {
		return nil, EditOutput{}, errors.New("owner and clear_owner are mutually exclusive")
	}
	if input.ClearPriority && input.Priority != nil {
		return nil, EditOutput{}, errors.New("priority and clear_priority are mutually exclusive")
	}
	if input.Parent != nil && input.RemoveParent != nil {
		return nil, EditOutput{}, errors.New("parent and remove_parent are mutually exclusive")
	}
	if input.Title != nil && strings.TrimSpace(*input.Title) == "" {
		return nil, EditOutput{}, errors.New("title must not be empty")
	}
	if err := validatePriority(input.Priority); err != nil {
		return nil, EditOutput{}, err
	}
	delta, err := h.linksDelta(ctx, project, input)
	if err != nil {
		return nil, EditOutput{}, err
	}
	metadataPatch, err := editMetadata(input)
	if err != nil {
		return nil, EditOutput{}, err
	}
	body := generated.EditIssueBody{
		Actor:         &h.options.Actor,
		Title:         input.Title,
		Body:          input.Body,
		Owner:         input.Owner,
		SetPriority:   input.Priority,
		ClearPriority: optionalTrue(input.ClearPriority),
		LinksDelta:    delta,
	}
	if input.ClearOwner {
		empty := ""
		body.Owner = &empty
	}
	hasIssueEdit := body.Title != nil || body.Body != nil || body.Owner != nil || body.SetPriority != nil || body.ClearPriority != nil || body.LinksDelta != nil
	if !hasIssueEdit && len(metadataPatch) == 0 {
		return nil, EditOutput{}, errors.New("edit requires at least one change")
	}
	if hasIssueEdit && len(metadataPatch) > 0 {
		return nil, EditOutput{}, errors.New("issue fields and metadata must be changed in separate calls")
	}
	var issue generated.Issue
	var changed bool
	var mainEvent *EventSummary
	var changes *LinkChangesSummary
	events := make([]EventSummary, 0)
	if hasIssueEdit {
		response, editErr := h.options.Client.EditIssue(ctx, &generated.EditIssueRequestOptions{
			PathParams: &generated.EditIssuePath{ProjectID: project.ID, Ref: ref},
			Body:       &body,
		})
		if editErr != nil {
			return nil, EditOutput{}, editErr
		}
		issue = response.Issue
		changed = response.Changed
		mainEvent = eventSummary(&response.Event)
		scopedChanges, changesErr := h.scopedLinkChanges(ctx, response.Changes)
		if changesErr != nil {
			return nil, EditOutput{}, changesErr
		}
		changes = linkChangesSummary(scopedChanges)
		for index := range response.Events {
			if event := eventSummary(&response.Events[index]); event != nil {
				events = append(events, *event)
			}
		}
	}
	if len(metadataPatch) > 0 {
		response, patchErr := h.options.Client.PatchIssueMetadata(ctx, &generated.PatchIssueMetadataRequestOptions{
			PathParams: &generated.PatchIssueMetadataPath{ProjectID: project.ID, Ref: ref},
			Body:       &generated.PatchIssueMetadataBody{Actor: &h.options.Actor, Patch: metadataPatch},
		})
		if patchErr != nil {
			return nil, EditOutput{}, patchErr
		}
		issue = response.Issue
		changed = changed || response.Changed
		if event := eventSummary(response.Event); event != nil {
			mainEvent = event
			events = append(events, *event)
		}
	}
	return successResult(), EditOutput{
		Project: project,
		Issue:   h.summaryFromIssue(project, issue),
		Changed: changed,
		Reused:  nil,
		Event:   mainEvent,
		Events:  events,
		Changes: changes,
	}, nil
}

func (h toolHandlers) comment(ctx context.Context, _ *sdkmcp.CallToolRequest, input CommentInput) (*sdkmcp.CallToolResult, CommentOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, CommentOutput{}, err
	}
	if strings.TrimSpace(input.Body) == "" {
		return nil, CommentOutput{}, errors.New("body must not be empty")
	}
	key := strings.TrimSpace(input.IdempotencyKey)
	if key == "" {
		return nil, CommentOutput{}, errors.New("idempotency_key must not be empty")
	}
	response, err := h.options.Client.CreateComment(ctx, &generated.CreateCommentRequestOptions{
		PathParams: &generated.CreateCommentPath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.CreateCommentBody{Actor: &h.options.Actor, Body: input.Body},
		Header:     &generated.CreateCommentHeaders{IdempotencyKey: &key},
	})
	if err != nil {
		return nil, CommentOutput{}, err
	}
	return successResult(), CommentOutput{
		Project: project,
		Issue:   h.summaryFromIssue(project, response.Issue),
		Comment: commentSummary(response.Comment),
		Changed: response.Changed,
		Reused:  optionalTrue(!response.Changed),
		Event:   eventSummary(&response.Event),
	}, nil
}

func (h toolHandlers) claim(ctx context.Context, _ *sdkmcp.CallToolRequest, input ClaimInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.ClaimIssue(ctx, &generated.ClaimIssueRequestOptions{
		PathParams: &generated.ClaimIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ClaimIssueBody{Actor: h.options.Actor, Force: optionalTrue(input.Force)},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	output := h.mutation(project, response.Issue, response.Changed, nil, response.Event)
	output.PreviousOwner = response.PreviousOwner
	return successResult(), output, nil
}

func (h toolHandlers) setLabel(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetLabelInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	label := strings.TrimSpace(input.Label)
	if label == "" {
		return nil, MutationOutput{}, errors.New("label must not be empty")
	}
	if input.Present {
		response, err := h.options.Client.AddLabel(ctx, &generated.AddLabelRequestOptions{
			PathParams: &generated.AddLabelPath{ProjectID: project.ID, Ref: ref},
			Body:       &generated.AddLabelBody{Actor: &h.options.Actor, Label: label},
		})
		if err != nil {
			return nil, MutationOutput{}, err
		}
		return successResult(), h.mutation(project, response.Issue, response.Changed, nil, &response.Event), nil
	}
	response, err := h.options.Client.RemoveLabel(ctx, &generated.RemoveLabelRequestOptions{
		PathParams: &generated.RemoveLabelPath{ProjectID: project.ID, Ref: ref, Label: label},
		Query:      &generated.RemoveLabelQuery{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) setDeadline(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetDeadlineInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	if input.Deadline != nil && input.ClearDeadline {
		return nil, MutationOutput{}, errors.New("deadline and clear_deadline are mutually exclusive")
	}
	if input.Deadline == nil && !input.ClearDeadline {
		return nil, MutationOutput{}, errors.New("set_deadline requires deadline or clear_deadline")
	}
	value := any(nil)
	if input.Deadline != nil {
		value = *input.Deadline
	}
	return h.patchMetadata(ctx, project, ref, map[string]any{"deadline_on": value}, input.Revision)
}

func (h toolHandlers) setSchedule(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetScheduleInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	if input.Schedule != nil && input.ClearSchedule {
		return nil, MutationOutput{}, errors.New("schedule and clear_schedule are mutually exclusive")
	}
	if input.Schedule == nil && !input.ClearSchedule {
		return nil, MutationOutput{}, errors.New("set_schedule requires schedule or clear_schedule")
	}
	value := any(nil)
	if input.Schedule != nil {
		value = *input.Schedule
	}
	return h.patchMetadata(ctx, project, ref, map[string]any{"scheduled_on": value}, input.Revision)
}

func (h toolHandlers) setMetadata(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetMetadataInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	if len(input.Patch) == 0 {
		return nil, MutationOutput{}, errors.New("patch must contain at least one metadata key")
	}
	for key := range input.Patch {
		if strings.TrimSpace(key) == "" {
			return nil, MutationOutput{}, errors.New("metadata keys must not be empty")
		}
	}
	return h.patchMetadata(ctx, project, ref, input.Patch, input.Revision)
}

func (h toolHandlers) patchMetadata(ctx context.Context, project ProjectIdentity, ref string, patch map[string]any, revision *int64) (*sdkmcp.CallToolResult, MutationOutput, error) {
	var headers *generated.PatchIssueMetadataHeaders
	if revision != nil {
		if *revision < 0 {
			return nil, MutationOutput{}, errors.New("revision must not be negative")
		}
		ifMatch := `"rev-` + strconv.FormatInt(*revision, 10) + `"`
		headers = &generated.PatchIssueMetadataHeaders{IfMatch: &ifMatch}
	}
	response, err := h.options.Client.PatchIssueMetadata(ctx, &generated.PatchIssueMetadataRequestOptions{
		PathParams: &generated.PatchIssueMetadataPath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.PatchIssueMetadataBody{Actor: &h.options.Actor, Patch: patch},
		Header:     headers,
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, nil, response.Event), nil
}

func (h toolHandlers) close(ctx context.Context, _ *sdkmcp.CallToolRequest, input CloseInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	reason := generated.ActionRequestBodyReason(input.Reason)
	if err := reason.Validate(); err != nil || reason == generated.Empty {
		return nil, MutationOutput{}, fmt.Errorf("reason must be one of done, wontfix, duplicate, superseded, or audit-no-change")
	}
	if strings.TrimSpace(input.Message) == "" {
		return nil, MutationOutput{}, errors.New("message must not be empty")
	}
	evidence := make([]generated.Evidence, 0, len(input.Evidence))
	for index, item := range input.Evidence {
		converted, err := h.convertEvidence(ctx, project, item)
		if err != nil {
			return nil, MutationOutput{}, fmt.Errorf("evidence %d: %w", index+1, err)
		}
		evidence = append(evidence, converted)
	}
	response, err := h.options.Client.CloseIssue(ctx, &generated.CloseIssueRequestOptions{
		PathParams: &generated.CloseIssuePath{ProjectID: project.ID, Ref: ref},
		Body: &generated.CloseIssueBody{
			Actor:    &h.options.Actor,
			Reason:   &reason,
			Message:  &input.Message,
			Evidence: evidence,
			DryRun:   optionalTrue(input.DryRun),
		},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) reopen(ctx context.Context, _ *sdkmcp.CallToolRequest, input ReopenInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	project, ref, err := h.options.Scope.IssueTarget(ctx, h.options.Client, input.Ref, true)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.ReopenIssue(ctx, &generated.ReopenIssueRequestOptions{
		PathParams: &generated.ReopenIssuePath{ProjectID: project.ID, Ref: ref},
		Body:       &generated.ReopenIssueBody{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(project, response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) createLinks(ctx context.Context, project ProjectIdentity, input CreateInput) ([]generated.CreateInitialLinkBody, error) {
	links := make([]generated.CreateInitialLinkBody, 0, 1+len(input.Blocks)+len(input.BlockedBy)+len(input.Related))
	add := func(ref string, linkType generated.CreateInitialLinkBodyType, incoming bool) error {
		bound, err := h.relationshipRef(ctx, project, ref)
		if err != nil {
			return err
		}
		var incomingPointer *bool
		if incoming {
			incomingPointer = &incoming
		}
		links = append(links, generated.CreateInitialLinkBody{Type: linkType, ToRef: bound, Incoming: incomingPointer})
		return nil
	}
	if input.Parent != "" {
		if err := add(input.Parent, generated.Parent, false); err != nil {
			return nil, err
		}
	}
	for _, ref := range input.Blocks {
		if err := add(ref, generated.Blocks, false); err != nil {
			return nil, err
		}
	}
	for _, ref := range input.BlockedBy {
		if err := add(ref, generated.Blocks, true); err != nil {
			return nil, err
		}
	}
	for _, ref := range input.Related {
		if err := add(ref, generated.Related, false); err != nil {
			return nil, err
		}
	}
	return links, nil
}

func (h toolHandlers) linksDelta(ctx context.Context, project ProjectIdentity, input EditInput) (*generated.LinksDelta, error) {
	delta := &generated.LinksDelta{}
	var changed bool
	setOne := func(target **string, raw string) error {
		ref, err := h.relationshipRef(ctx, project, raw)
		if err != nil {
			return err
		}
		*target = &ref
		changed = true
		return nil
	}
	setMany := func(target *[]string, raw []string) error {
		for _, item := range raw {
			ref, err := h.relationshipRef(ctx, project, item)
			if err != nil {
				return err
			}
			*target = append(*target, ref)
			changed = true
		}
		return nil
	}
	if input.Parent != nil {
		if err := setOne(&delta.SetParent, *input.Parent); err != nil {
			return nil, err
		}
	}
	if input.RemoveParent != nil {
		if err := setOne(&delta.RemoveParent, *input.RemoveParent); err != nil {
			return nil, err
		}
	}
	for target, refs := range map[*[]string][]string{
		&delta.AddBlocks:       input.AddBlocks,
		&delta.RemoveBlocks:    input.RemoveBlocks,
		&delta.AddBlockedBy:    input.AddBlockedBy,
		&delta.RemoveBlockedBy: input.RemoveBlockedBy,
		&delta.AddRelated:      input.AddRelated,
		&delta.RemoveRelated:   input.RemoveRelated,
	} {
		if err := setMany(target, refs); err != nil {
			return nil, err
		}
	}
	if !changed {
		return nil, nil
	}
	return delta, nil
}

func (h toolHandlers) boundRelationshipRef(raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", errors.New("relationship reference must not be empty")
	}
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("relationship reference %q is invalid", ref)
	}
	if parsed.ULID != "" {
		return "", fmt.Errorf("relationship reference %q is an unscoped UID; use a bound-project short reference", ref)
	}
	if len(parsed.ShortID) == shortid.MaxLength {
		return "", fmt.Errorf("relationship reference %q uses an ambiguous full-length short ID; use the Kata CLI for this target", ref)
	}
	if parsed.Project != "" && parsed.Project != h.options.ProjectName {
		return "", fmt.Errorf("relationship reference %q is outside bound project %q", ref, h.options.ProjectName)
	}
	return parsed.ShortID, nil
}

func (h toolHandlers) readProjects(ctx context.Context, selector string) ([]ProjectIdentity, error) {
	if strings.TrimSpace(selector) != "" {
		project, err := h.options.Scope.Project(ctx, h.options.Client, selector, false)
		if err != nil {
			return nil, err
		}
		return []ProjectIdentity{project}, nil
	}
	return h.options.Scope.Projects(ctx, h.options.Client, false)
}

func (h toolHandlers) createTarget(ctx context.Context, selector string) (ProjectIdentity, error) {
	selector = strings.TrimSpace(selector)
	if h.options.Scope.Mode() != ScopeBound && selector == "" {
		return ProjectIdentity{}, errors.New("multi-project creates require project")
	}
	return h.options.Scope.Project(ctx, h.options.Client, selector, false)
}

func createMetadata(input CreateInput) (map[string]any, error) {
	patch := copyMetadata(input.Metadata)
	if input.ScheduledOn != "" {
		if _, exists := patch["scheduled_on"]; exists {
			return nil, errors.New("scheduled_on is present in both the dedicated field and metadata")
		}
		patch["scheduled_on"] = input.ScheduledOn
	}
	if input.Timezone != "" {
		if _, exists := patch["timezone"]; exists {
			return nil, errors.New("timezone is present in both the dedicated field and metadata")
		}
		patch["timezone"] = input.Timezone
	}
	for key, value := range patch {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
		if err := metadata.ValidateCreateValue(metadata.IssueRegistry, key, raw); err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, nil
}

func editMetadata(input EditInput) (map[string]any, error) {
	patch := copyMetadata(input.Metadata)
	if input.ScheduledOn != nil && input.ClearScheduledOn {
		return nil, errors.New("scheduled_on and clear_scheduled_on are mutually exclusive")
	}
	if input.Timezone != nil && input.ClearTimezone {
		return nil, errors.New("timezone and clear_timezone are mutually exclusive")
	}
	if input.ScheduledOn != nil || input.ClearScheduledOn {
		if _, exists := patch["scheduled_on"]; exists {
			return nil, errors.New("scheduled_on is present in both the dedicated field and metadata")
		}
		if input.ClearScheduledOn {
			patch["scheduled_on"] = nil
		} else {
			patch["scheduled_on"] = *input.ScheduledOn
		}
	}
	if input.Timezone != nil || input.ClearTimezone {
		if _, exists := patch["timezone"]; exists {
			return nil, errors.New("timezone is present in both the dedicated field and metadata")
		}
		if input.ClearTimezone {
			patch["timezone"] = nil
		} else {
			patch["timezone"] = *input.Timezone
		}
	}
	for key, value := range patch {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
		if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
			return nil, fmt.Errorf("metadata %q: %w", key, err)
		}
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, nil
}

func copyMetadata(source map[string]any) map[string]any {
	if len(source) == 0 {
		return make(map[string]any)
	}
	result := make(map[string]any, len(source)+2)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (h toolHandlers) relationshipRef(ctx context.Context, source ProjectIdentity, raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", errors.New("relationship reference must not be empty")
	}
	parsed, err := shortid.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("relationship reference %q is invalid", ref)
	}
	if parsed.ULID != "" {
		return "", fmt.Errorf("relationship reference %q is an unscoped UID; use a project-qualified short reference", ref)
	}
	if len(parsed.ShortID) == shortid.MaxLength {
		return "", fmt.Errorf("relationship reference %q uses an ambiguous full-length short ID; use a shorter project-qualified reference", ref)
	}
	if h.options.Scope.Mode() == ScopeBound {
		if parsed.Project != "" && parsed.Project != source.Name {
			return "", fmt.Errorf("relationship reference %q is outside bound project %q", ref, source.Name)
		}
		return parsed.ShortID, nil
	}
	if parsed.Project == "" {
		return "", errors.New("multi-project relationships require project#ref")
	}
	if _, err := h.options.Scope.Project(ctx, h.options.Client, parsed.Project, false); err != nil {
		return "", err
	}
	return parsed.Project + "#" + parsed.ShortID, nil
}

func outputProjectScope(projects []ProjectIdentity) (ProjectIdentity, []ProjectIdentity) {
	if len(projects) == 1 {
		return projects[0], nil
	}
	return ProjectIdentity{}, projects
}

func projectIDSet(projects []ProjectIdentity) map[int64]struct{} {
	result := make(map[int64]struct{}, len(projects))
	for _, project := range projects {
		result[project.ID] = struct{}{}
	}
	return result
}

func (h toolHandlers) summaryFromIssue(project ProjectIdentity, issue generated.Issue) IssueSummary {
	return IssueSummary{
		UID:          issue.UID,
		Ref:          issue.ShortID,
		QualifiedRef: project.Name + "#" + issue.ShortID,
		Title:        issue.Title,
		Status:       issue.Status,
		Owner:        issue.Owner,
		Priority:     issue.Priority,
		Revision:     issue.Revision,
		UpdatedAt:    formatTime(issue.UpdatedAt),
		ScheduledOn:  metadataString(issue.Metadata, "scheduled_on"),
		Timezone:     metadataString(issue.Metadata, "timezone"),
	}
}

func (h toolHandlers) summaryFromIssueOut(project ProjectIdentity, issue generated.IssueOut) IssueSummary {
	qualified := issue.QualifiedID
	if qualified == "" {
		qualified = project.Name + "#" + issue.ShortID
	}
	return IssueSummary{
		UID:          issue.UID,
		Ref:          issue.ShortID,
		QualifiedRef: qualified,
		Title:        issue.Title,
		Status:       issue.Status,
		Owner:        issue.Owner,
		Priority:     issue.Priority,
		Labels:       stringSlicePointer(nonNilStrings(issue.Labels)),
		Blocked:      issue.Blocked,
		Revision:     issue.Revision,
		UpdatedAt:    formatTime(issue.UpdatedAt),
		ScheduledOn:  metadataString(issue.Metadata, "scheduled_on"),
		Timezone:     metadataString(issue.Metadata, "timezone"),
	}
}

func summaryFromGlobalIssue(issue generated.ListGlobalIssueOut) IssueSummary {
	return IssueSummary{
		UID: issue.UID, Ref: issue.ShortID, QualifiedRef: issue.QualifiedID,
		Title: issue.Title, Status: issue.Status, Owner: issue.Owner, Priority: issue.Priority,
		Labels: stringSlicePointer(nonNilStrings(issue.Labels)), Blocked: issue.Blocked,
		Revision: issue.Revision, UpdatedAt: formatTime(issue.UpdatedAt),
		ScheduledOn: metadataString(issue.Metadata, "scheduled_on"), Timezone: metadataString(issue.Metadata, "timezone"),
	}
}

func summaryFromReadyGlobalIssue(issue generated.ReadyGlobalIssueOut) IssueSummary {
	return IssueSummary{
		UID: issue.UID, Ref: issue.ShortID, QualifiedRef: issue.QualifiedID,
		Title: issue.Title, Status: issue.Status, Owner: issue.Owner, Priority: issue.Priority,
		Labels: stringSlicePointer(nonNilStrings(issue.Labels)), Blocked: issue.Blocked,
		Revision: issue.Revision, UpdatedAt: formatTime(issue.UpdatedAt),
		ScheduledOn: metadataString(issue.Metadata, "scheduled_on"), Timezone: metadataString(issue.Metadata, "timezone"),
	}
}

func metadataString(values map[string]any, key string) *string {
	value, ok := values[key].(string)
	if !ok {
		return nil
	}
	return &value
}

func (h toolHandlers) mutation(project ProjectIdentity, issue generated.Issue, changed bool, reused *bool, event *generated.Event) MutationOutput {
	return MutationOutput{
		Project: project,
		Issue:   h.summaryFromIssue(project, issue),
		Changed: changed,
		Reused:  reused,
		Event:   eventSummary(event),
	}
}

func eventSummary(event *generated.Event) *EventSummary {
	if event == nil || event.UID == "" {
		return nil
	}
	return &EventSummary{UID: event.UID, Type: event.Type, Actor: event.Actor}
}

func commentSummary(comment generated.Comment) CommentSummary {
	return CommentSummary{
		UID:       comment.UID,
		Author:    comment.Author,
		Body:      comment.Body,
		CreatedAt: formatTime(comment.CreatedAt),
	}
}

func linkSummary(issueUID string, link generated.LinkOut) LinkSummary {
	peer, typeName := linkPeerAndType(issueUID, link)
	return LinkSummary{
		Type:         typeName,
		QualifiedRef: peer.QualifiedID,
		Status:       peer.Status,
	}
}

func linkPeerAndType(issueUID string, link generated.LinkOut) (generated.LinkPeer, string) {
	peer := link.To
	typeName := link.Type
	if link.To.UID == issueUID {
		peer = link.From
		switch link.Type {
		case "blocks":
			typeName = "blocked_by"
		case "parent":
			typeName = "child"
		}
	}
	return peer, typeName
}

// scopedLinkChanges removes relationship-change peers in projects outside
// the startup scope, such as a replaced parent that lived in a foreign
// project, before the deltas reach the client.
func (h toolHandlers) scopedLinkChanges(ctx context.Context, changes *generated.LinkChanges) (*generated.LinkChanges, error) {
	if changes == nil || h.options.Scope.Mode() == ScopeAll {
		return changes, nil
	}
	projects, err := h.options.Scope.Projects(ctx, h.options.Client, true)
	if err != nil {
		return nil, err
	}
	inScope := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		inScope[project.Name] = struct{}{}
	}
	keep := func(peers []generated.LinkPeer) []generated.LinkPeer {
		kept := make([]generated.LinkPeer, 0, len(peers))
		for _, peer := range peers {
			if _, ok := inScope[peer.Project]; ok {
				kept = append(kept, peer)
			}
		}
		if len(kept) == 0 {
			return nil
		}
		return kept
	}
	keepOne := func(peer *generated.LinkPeer) *generated.LinkPeer {
		if peer == nil {
			return nil
		}
		if _, ok := inScope[peer.Project]; !ok {
			return nil
		}
		return peer
	}
	changes.BlockedByAdded = keep(changes.BlockedByAdded)
	changes.BlockedByRemoved = keep(changes.BlockedByRemoved)
	changes.BlocksAdded = keep(changes.BlocksAdded)
	changes.BlocksRemoved = keep(changes.BlocksRemoved)
	changes.ParentRemoved = keepOne(changes.ParentRemoved)
	changes.ParentSet = keepOne(changes.ParentSet)
	changes.RelatedAdded = keep(changes.RelatedAdded)
	changes.RelatedRemoved = keep(changes.RelatedRemoved)
	return changes, nil
}

func linkChangesSummary(changes *generated.LinkChanges) *LinkChangesSummary {
	if changes == nil {
		return nil
	}
	return &LinkChangesSummary{
		BlockedByAdded:   linkPeerSummaries(changes.BlockedByAdded),
		BlockedByRemoved: linkPeerSummaries(changes.BlockedByRemoved),
		BlocksAdded:      linkPeerSummaries(changes.BlocksAdded),
		BlocksRemoved:    linkPeerSummaries(changes.BlocksRemoved),
		ParentRemoved:    linkPeerSummary(changes.ParentRemoved),
		ParentSet:        linkPeerSummary(changes.ParentSet),
		RelatedAdded:     linkPeerSummaries(changes.RelatedAdded),
		RelatedRemoved:   linkPeerSummaries(changes.RelatedRemoved),
	}
}

func linkPeerSummaries(peers []generated.LinkPeer) []LinkPeerSummary {
	if len(peers) == 0 {
		return nil
	}
	result := make([]LinkPeerSummary, 0, len(peers))
	for index := range peers {
		result = append(result, *linkPeerSummary(&peers[index]))
	}
	return result
}

func linkPeerSummary(peer *generated.LinkPeer) *LinkPeerSummary {
	if peer == nil {
		return nil
	}
	return &LinkPeerSummary{
		UID:          peer.UID,
		Ref:          peer.ShortID,
		QualifiedRef: peer.QualifiedID,
		Status:       peer.Status,
	}
}

func (h toolHandlers) convertEvidence(ctx context.Context, project ProjectIdentity, input Evidence) (generated.Evidence, error) {
	typeName := strings.TrimSpace(input.Type)
	allowed := map[string]bool{
		"commit": true, "pr": true, "test": true, "reviewed-paths": true,
		"no-change-audit": true, "duplicate-of": true, "superseded-by": true,
	}
	if !allowed[typeName] {
		return generated.Evidence{}, fmt.Errorf("unknown type %q", typeName)
	}
	paths := compactStrings(input.Paths)
	result := generated.Evidence{Type: typeName, Paths: paths}
	result.Sha = optionalString(input.SHA)
	result.URL = optionalString(input.URL)
	result.Command = optionalString(input.Command)
	result.Rationale = optionalString(input.Rationale)
	if input.IssueRef != "" {
		ref, err := h.relationshipRef(ctx, project, input.IssueRef)
		if err != nil {
			return generated.Evidence{}, err
		}
		result.IssueRef = &ref
	}
	switch typeName {
	case "commit":
		if result.Sha == nil {
			return generated.Evidence{}, errors.New("commit evidence requires sha")
		}
	case "pr":
		if result.URL == nil {
			return generated.Evidence{}, errors.New("pr evidence requires url")
		}
	case "test":
		if result.Command == nil {
			return generated.Evidence{}, errors.New("test evidence requires command")
		}
	case "reviewed-paths":
		if len(result.Paths) == 0 {
			return generated.Evidence{}, errors.New("reviewed-paths evidence requires paths")
		}
	case "no-change-audit":
		if result.Rationale == nil {
			return generated.Evidence{}, errors.New("no-change-audit evidence requires rationale")
		}
	case "duplicate-of", "superseded-by":
		if result.IssueRef == nil {
			return generated.Evidence{}, fmt.Errorf("%s evidence requires issue_ref", typeName)
		}
	}
	return result, nil
}

func successResult() *sdkmcp.CallToolResult {
	return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{}}
}

func boundedLimit(value int) (int, error) {
	if value == 0 {
		return defaultResultLimit, nil
	}
	if value < 1 || value > maximumResultLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maximumResultLimit)
	}
	return value, nil
}

func validatePriority(priority *int64) error {
	if priority != nil && (*priority < 0 || *priority > 4) {
		return errors.New("priority must be between 0 and 4")
	}
	return nil
}

func priorityQuery(priority *int64) (*string, error) {
	if err := validatePriority(priority); err != nil {
		return nil, err
	}
	if priority == nil {
		return nil, nil
	}
	value := strconv.FormatInt(*priority, 10)
	return &value, nil
}

func optionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func optionalTrue(value bool) *bool {
	if !value {
		return nil
	}
	return &value
}

func compactStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func stringSlicePointer(values []string) *[]string {
	return &values
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
