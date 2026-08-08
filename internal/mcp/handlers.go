package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/oklog/ulid/v2"

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
	limit64 := int64(limit + 1)
	response, err := h.options.Client.SearchIssues(ctx, &generated.SearchIssuesRequestOptions{
		PathParams: &generated.SearchIssuesPath{ProjectID: h.options.ProjectID},
		Query: &generated.SearchIssuesQuery{
			Q:            query,
			Limit:        &limit64,
			Mode:         mode,
			Label:        compactStrings(input.Labels),
			ExcludeLabel: compactStrings(input.ExcludeLabels),
		},
	})
	if err != nil {
		return nil, SearchOutput{}, err
	}
	truncated := len(response.Results) > limit
	hits := response.Results
	if truncated {
		hits = hits[:limit]
	}
	results := make([]SearchHit, 0, len(hits))
	for _, hit := range hits {
		results = append(results, SearchHit{
			Issue:     h.summaryFromIssue(hit.Issue),
			Score:     hit.Score,
			MatchedIn: nonNilStrings(hit.MatchedIn),
		})
	}
	degraded := response.Degraded != nil && *response.Degraded
	degradedReason := ""
	if response.DegradedReason != nil {
		degradedReason = *response.DegradedReason
	}
	return successResult(), SearchOutput{
		Project:        h.project(),
		Query:          response.Query,
		Mode:           response.Mode,
		Degraded:       degraded,
		DegradedReason: degradedReason,
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
	if input.Status != "" {
		value := generated.ListIssuesQueryStatus(input.Status)
		if err := value.Validate(); err != nil {
			return nil, IssueListOutput{}, fmt.Errorf("status: %w", err)
		}
		status = &value
	}
	priority, err := priorityQuery(input.Priority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	maxPriority, err := priorityQuery(input.MaxPriority)
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	limit64 := int64(limit + 1)
	owner := optionalString(input.Owner)
	unowned := optionalTrue(input.Unowned)
	response, err := h.options.Client.ListIssues(ctx, &generated.ListIssuesRequestOptions{
		PathParams: &generated.ListIssuesPath{ProjectID: h.options.ProjectID},
		Query: &generated.ListIssuesQuery{
			Status:       status,
			Priority:     priority,
			MaxPriority:  maxPriority,
			Limit:        &limit64,
			Unowned:      unowned,
			Owner:        owner,
			Label:        compactStrings(input.Labels),
			ExcludeLabel: compactStrings(input.ExcludeLabels),
			Meta:         compactStrings(input.Metadata),
		},
	})
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	truncated := len(response.Issues) > limit
	responseIssues := response.Issues
	if truncated {
		responseIssues = responseIssues[:limit]
	}
	issues := make([]IssueSummary, 0, len(responseIssues))
	for _, issue := range responseIssues {
		issues = append(issues, h.summaryFromIssueOut(issue))
	}
	return successResult(), IssueListOutput{
		Project:   h.project(),
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
	limit64 := int64(limit + 1)
	response, err := h.options.Client.ReadyIssues(ctx, &generated.ReadyIssuesRequestOptions{
		PathParams: &generated.ReadyIssuesPath{ProjectID: h.options.ProjectID},
		Query: &generated.ReadyIssuesQuery{
			Limit:        &limit64,
			Unowned:      optionalTrue(input.Unowned),
			Owner:        optionalString(input.Owner),
			Label:        compactStrings(input.Labels),
			ExcludeLabel: compactStrings(input.ExcludeLabels),
		},
	})
	if err != nil {
		return nil, IssueListOutput{}, err
	}
	truncated := len(response.Issues) > limit
	responseIssues := response.Issues
	if truncated {
		responseIssues = responseIssues[:limit]
	}
	issues := make([]IssueSummary, 0, len(responseIssues))
	for _, issue := range responseIssues {
		issues = append(issues, h.summaryFromIssueOut(issue))
	}
	return successResult(), IssueListOutput{
		Project:   h.project(),
		Issues:    issues,
		Truncated: truncated,
	}, nil
}

func (h toolHandlers) labels(ctx context.Context, _ *sdkmcp.CallToolRequest, _ LabelsInput) (*sdkmcp.CallToolResult, LabelsOutput, error) {
	response, err := h.options.Client.ListLabels(ctx, &generated.ListLabelsRequestOptions{
		PathParams: &generated.ListLabelsPath{ProjectID: h.options.ProjectID},
	})
	if err != nil {
		return nil, LabelsOutput{}, err
	}
	labels := make([]LabelCount, 0, len(response.Labels))
	for _, label := range response.Labels {
		labels = append(labels, LabelCount{Label: label.Label, Count: label.Count})
	}
	return successResult(), LabelsOutput{Project: h.project(), Labels: labels}, nil
}

func (h toolHandlers) show(ctx context.Context, _ *sdkmcp.CallToolRequest, input ShowInput) (*sdkmcp.CallToolResult, ShowOutput, error) {
	ref, err := h.boundRef(input.Ref)
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
		PathParams: &generated.ShowIssuePath{ProjectID: h.options.ProjectID, Ref: ref},
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
	for _, link := range response.Links {
		links = append(links, linkSummary(response.Issue.UID, link))
	}
	summary := h.summaryFromIssue(response.Issue)
	metadata := response.Issue.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	return successResult(), ShowOutput{
		Project: h.project(),
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
	links, err := h.createLinks(input)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	body := generated.CreateIssueBody{
		Actor:    &h.options.Actor,
		Title:    title,
		Labels:   compactStrings(input.Labels),
		Links:    links,
		Metadata: input.Metadata,
		Priority: input.Priority,
	}
	if input.Body != "" {
		body.Body = &input.Body
	}
	if strings.TrimSpace(input.Owner) != "" {
		body.Owner = &input.Owner
	}
	response, err := h.options.Client.CreateIssue(ctx, &generated.CreateIssueRequestOptions{
		PathParams: &generated.CreateIssuePath{ProjectID: h.options.ProjectID},
		Body:       &body,
		Header:     &generated.CreateIssueHeaders{IdempotencyKey: &key},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) edit(ctx context.Context, _ *sdkmcp.CallToolRequest, input EditInput) (*sdkmcp.CallToolResult, EditOutput, error) {
	ref, err := h.boundRef(input.Ref)
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
	delta, err := h.linksDelta(input)
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
	if body.Title == nil && body.Body == nil && body.Owner == nil && body.SetPriority == nil && body.ClearPriority == nil && body.LinksDelta == nil {
		return nil, EditOutput{}, errors.New("edit requires at least one change")
	}
	response, err := h.options.Client.EditIssue(ctx, &generated.EditIssueRequestOptions{
		PathParams: &generated.EditIssuePath{ProjectID: h.options.ProjectID, Ref: ref},
		Body:       &body,
	})
	if err != nil {
		return nil, EditOutput{}, err
	}
	events := make([]EventSummary, 0, len(response.Events))
	for index := range response.Events {
		if event := eventSummary(&response.Events[index]); event != nil {
			events = append(events, *event)
		}
	}
	return successResult(), EditOutput{
		Project: h.project(),
		Issue:   h.summaryFromIssue(response.Issue),
		Changed: response.Changed,
		Reused:  nil,
		Event:   eventSummary(&response.Event),
		Events:  events,
		Changes: linkChangesSummary(response.Changes),
	}, nil
}

func (h toolHandlers) comment(ctx context.Context, _ *sdkmcp.CallToolRequest, input CommentInput) (*sdkmcp.CallToolResult, CommentOutput, error) {
	ref, err := h.boundRef(input.Ref)
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
		PathParams: &generated.CreateCommentPath{ProjectID: h.options.ProjectID, Ref: ref},
		Body:       &generated.CreateCommentBody{Actor: &h.options.Actor, Body: input.Body},
		Header:     &generated.CreateCommentHeaders{IdempotencyKey: &key},
	})
	if err != nil {
		return nil, CommentOutput{}, err
	}
	return successResult(), CommentOutput{
		Project: h.project(),
		Issue:   h.summaryFromIssue(response.Issue),
		Comment: commentSummary(response.Comment),
		Changed: response.Changed,
		Reused:  optionalTrue(!response.Changed),
		Event:   eventSummary(&response.Event),
	}, nil
}

func (h toolHandlers) claim(ctx context.Context, _ *sdkmcp.CallToolRequest, input ClaimInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	ref, err := h.boundRef(input.Ref)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.ClaimIssue(ctx, &generated.ClaimIssueRequestOptions{
		PathParams: &generated.ClaimIssuePath{ProjectID: h.options.ProjectID, Ref: ref},
		Body:       &generated.ClaimIssueBody{Actor: h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(response.Issue, response.Changed, nil, response.Event), nil
}

func (h toolHandlers) setLabel(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetLabelInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	ref, err := h.boundRef(input.Ref)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	label := strings.TrimSpace(input.Label)
	if label == "" {
		return nil, MutationOutput{}, errors.New("label must not be empty")
	}
	if input.Present {
		response, err := h.options.Client.AddLabel(ctx, &generated.AddLabelRequestOptions{
			PathParams: &generated.AddLabelPath{ProjectID: h.options.ProjectID, Ref: ref},
			Body:       &generated.AddLabelBody{Actor: &h.options.Actor, Label: label},
		})
		if err != nil {
			return nil, MutationOutput{}, err
		}
		return successResult(), h.mutation(response.Issue, response.Changed, nil, &response.Event), nil
	}
	response, err := h.options.Client.RemoveLabel(ctx, &generated.RemoveLabelRequestOptions{
		PathParams: &generated.RemoveLabelPath{ProjectID: h.options.ProjectID, Ref: ref, Label: label},
		Query:      &generated.RemoveLabelQuery{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) setMetadata(ctx context.Context, _ *sdkmcp.CallToolRequest, input SetMetadataInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	ref, err := h.boundRef(input.Ref)
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
	var headers *generated.PatchIssueMetadataHeaders
	if input.Revision != nil {
		if *input.Revision < 0 {
			return nil, MutationOutput{}, errors.New("revision must not be negative")
		}
		ifMatch := "rev-" + strconv.FormatInt(*input.Revision, 10)
		headers = &generated.PatchIssueMetadataHeaders{IfMatch: &ifMatch}
	}
	response, err := h.options.Client.PatchIssueMetadata(ctx, &generated.PatchIssueMetadataRequestOptions{
		PathParams: &generated.PatchIssueMetadataPath{ProjectID: h.options.ProjectID, Ref: ref},
		Body:       &generated.PatchIssueMetadataBody{Actor: &h.options.Actor, Patch: input.Patch},
		Header:     headers,
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(response.Issue, response.Changed, nil, response.Event), nil
}

func (h toolHandlers) close(ctx context.Context, _ *sdkmcp.CallToolRequest, input CloseInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	ref, err := h.boundRef(input.Ref)
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
		converted, err := h.convertEvidence(item)
		if err != nil {
			return nil, MutationOutput{}, fmt.Errorf("evidence %d: %w", index+1, err)
		}
		evidence = append(evidence, converted)
	}
	response, err := h.options.Client.CloseIssue(ctx, &generated.CloseIssueRequestOptions{
		PathParams: &generated.CloseIssuePath{ProjectID: h.options.ProjectID, Ref: ref},
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
	return successResult(), h.mutation(response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) reopen(ctx context.Context, _ *sdkmcp.CallToolRequest, input ReopenInput) (*sdkmcp.CallToolResult, MutationOutput, error) {
	ref, err := h.boundRef(input.Ref)
	if err != nil {
		return nil, MutationOutput{}, err
	}
	response, err := h.options.Client.ReopenIssue(ctx, &generated.ReopenIssueRequestOptions{
		PathParams: &generated.ReopenIssuePath{ProjectID: h.options.ProjectID, Ref: ref},
		Body:       &generated.ReopenIssueBody{Actor: &h.options.Actor},
	})
	if err != nil {
		return nil, MutationOutput{}, err
	}
	return successResult(), h.mutation(response.Issue, response.Changed, response.Reused, &response.Event), nil
}

func (h toolHandlers) createLinks(input CreateInput) ([]generated.CreateInitialLinkBody, error) {
	links := make([]generated.CreateInitialLinkBody, 0, 1+len(input.Blocks)+len(input.BlockedBy)+len(input.Related))
	add := func(ref string, linkType generated.CreateInitialLinkBodyType, incoming bool) error {
		bound, err := h.boundRelationshipRef(ref)
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

func (h toolHandlers) linksDelta(input EditInput) (*generated.LinksDelta, error) {
	delta := &generated.LinksDelta{}
	var changed bool
	setOne := func(target **string, raw string) error {
		ref, err := h.boundRelationshipRef(raw)
		if err != nil {
			return err
		}
		*target = &ref
		changed = true
		return nil
	}
	setMany := func(target *[]string, raw []string) error {
		for _, item := range raw {
			ref, err := h.boundRelationshipRef(item)
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

func (h toolHandlers) boundRef(raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return "", errors.New("issue reference must not be empty")
	}
	if project, _, qualified := strings.Cut(ref, "#"); qualified && project != h.options.ProjectName {
		return "", fmt.Errorf("issue reference %q is outside bound project %q", ref, h.options.ProjectName)
	}
	return ref, nil
}

func (h toolHandlers) boundRelationshipRef(raw string) (string, error) {
	ref, err := h.boundRef(raw)
	if err != nil {
		return "", err
	}
	if len(ref) == 26 {
		if _, err := ulid.ParseStrict(strings.ToUpper(ref)); err == nil {
			return "", fmt.Errorf("relationship reference %q is an unscoped UID; use a bound-project short reference", ref)
		}
	}
	return ref, nil
}

func (h toolHandlers) project() ProjectIdentity {
	return ProjectIdentity{ID: h.options.ProjectID, Name: h.options.ProjectName}
}

func (h toolHandlers) summaryFromIssue(issue generated.Issue) IssueSummary {
	return IssueSummary{
		UID:          issue.UID,
		Ref:          issue.ShortID,
		QualifiedRef: h.options.ProjectName + "#" + issue.ShortID,
		Title:        issue.Title,
		Status:       issue.Status,
		Owner:        issue.Owner,
		Priority:     issue.Priority,
		Revision:     issue.Revision,
		UpdatedAt:    formatTime(issue.UpdatedAt),
	}
}

func (h toolHandlers) summaryFromIssueOut(issue generated.IssueOut) IssueSummary {
	qualified := issue.QualifiedID
	if qualified == "" {
		qualified = h.options.ProjectName + "#" + issue.ShortID
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
	}
}

func (h toolHandlers) mutation(issue generated.Issue, changed bool, reused *bool, event *generated.Event) MutationOutput {
	return MutationOutput{
		Project: h.project(),
		Issue:   h.summaryFromIssue(issue),
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
	return LinkSummary{
		Type:         typeName,
		QualifiedRef: peer.QualifiedID,
		Status:       peer.Status,
	}
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

func (h toolHandlers) convertEvidence(input Evidence) (generated.Evidence, error) {
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
		ref, err := h.boundRelationshipRef(input.IssueRef)
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
