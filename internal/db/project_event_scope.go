package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
)

// ProjectScopeResetEventType is a storage projection marker for a visible
// event whose structured references cross the caller's project boundary.
const ProjectScopeResetEventType = "sync.reset_required"

type federationEventStreamScopeKey struct{}

// WithFederationEventStream marks an authenticated federation poll. The
// request is already limited to the enrolled source project, and its trusted
// peer needs cross-project relationship UIDs to converge links after both
// projects reach the same hub.
func WithFederationEventStream(ctx context.Context) context.Context {
	return context.WithValue(ctx, federationEventStreamScopeKey{}, true)
}

func federationEventStream(ctx context.Context) bool {
	allowed, _ := ctx.Value(federationEventStreamScopeKey{}).(bool)
	return allowed
}

// EventRequiresProjectScopeReset detects structured references that cannot be
// released to a project-scoped reader. The lookup must honor ctx's authorized
// project boundary so inaccessible issue identities resolve as not found.
func EventRequiresProjectScopeReset(
	ctx context.Context,
	event Event,
	issueByUID func(string) (Issue, error),
	issueByRef func(string, string) (Issue, error),
) (bool, error) {
	if federationEventStream(ctx) {
		return false, nil
	}
	allowedUIDs, restricted := AuthorizedProjects(ctx)
	if !restricted {
		return false, nil
	}
	allowed := make(map[string]struct{}, len(allowedUIDs))
	for _, uid := range allowedUIDs {
		allowed[uid] = struct{}{}
	}
	issueIsPrivate := func(uid string) (bool, error) {
		if uid == "" || issueByUID == nil {
			return true, nil
		}
		issue, err := issueByUID(uid)
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		_, visible := allowed[issue.ProjectUID]
		return !visible, nil
	}
	for _, relatedUID := range []*string{event.RelatedIssueUID} {
		if relatedUID == nil {
			continue
		}
		private, err := issueIsPrivate(*relatedUID)
		if err != nil || private {
			return private, err
		}
	}
	checkUIDs := func(uids []string) (bool, error) {
		for _, uid := range uids {
			private, err := issueIsPrivate(uid)
			if err != nil || private {
				return private, err
			}
		}
		return false, nil
	}
	checkRefs := func(refs []string) (bool, error) {
		for _, ref := range refs {
			if ref == "" || issueByRef == nil {
				return true, nil
			}
			issue, err := issueByRef(event.ProjectUID, ref)
			if errors.Is(err, ErrNotFound) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			if _, visible := allowed[issue.ProjectUID]; !visible {
				return true, nil
			}
		}
		return false, nil
	}

	switch event.Type {
	case "issue.moved":
		var payload map[string]jsontext.Value
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		for _, key := range []string{"from_project_uid", "to_project_uid"} {
			uid, ok := StringValue(payload[key])
			if !ok || uid == "" {
				return true, nil
			}
			if _, visible := allowed[uid]; !visible {
				return true, nil
			}
		}
		return false, nil
	case "issue.created", "issue.snapshot":
		var payload map[string]jsontext.Value
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		rawLinks, found := payload["links"]
		if !found {
			return false, nil
		}
		var links *[]map[string]jsontext.Value
		if err := json.Unmarshal(rawLinks, &links); err != nil || links == nil {
			return true, nil
		}
		uids := make([]string, 0, len(*links))
		for _, link := range *links {
			uid, ok := StringValue(link["to_issue_uid"])
			if !ok || uid == "" {
				return true, nil
			}
			uids = append(uids, uid)
		}
		return checkUIDs(uids)
	case "issue.closed":
		var payload struct {
			ParentUID     *string    `json:"parent_uid"`
			ParentShortID *string    `json:"parent_short_id"`
			Evidence      []Evidence `json:"evidence"`
		}
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		if payload.ParentUID == nil || *payload.ParentUID == "" {
			if payload.ParentShortID != nil && *payload.ParentShortID != "" {
				return true, nil
			}
		} else {
			private, err := issueIsPrivate(*payload.ParentUID)
			if err != nil || private {
				return private, err
			}
		}
		refs := make([]string, 0, len(payload.Evidence))
		for _, evidence := range payload.Evidence {
			if evidence.Type == "duplicate-of" || evidence.Type == "superseded-by" {
				refs = append(refs, evidence.IssueRef)
			}
		}
		return checkRefs(refs)
	case "issue.links_changed":
		var payload map[string]jsontext.Value
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		var uids []string
		for _, key := range []string{
			"parent_set_uid", "parent_removed_uid", "blocks_added_uids", "blocks_removed_uids",
			"blocked_by_added_uids", "blocked_by_removed_uids", "related_added_uids", "related_removed_uids",
		} {
			raw, found := payload[key]
			if !found {
				continue
			}
			var values []string
			if err := json.Unmarshal(raw, &values); err != nil {
				var single string
				if singleErr := json.Unmarshal(raw, &single); singleErr != nil || single == "" {
					return true, nil
				}
				values = []string{single}
			}
			uids = append(uids, values...)
		}
		return checkUIDs(uids)
	case "close.throttled":
		var payload CloseThrottledPayload
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		refs := []string{payload.Parent}
		refs = append(refs, payload.Cohort...)
		if payload.Prior != nil {
			refs = append(refs, *payload.Prior)
		}
		return checkRefs(refs)
	default:
		return false, nil
	}
}

// ProjectScopeResetEvent retains only the visible project and durable cursor
// needed to invalidate a scoped reader. It carries no source or issue identity.
func ProjectScopeResetEvent(event Event) Event {
	return Event{
		ID: event.ID, Type: ProjectScopeResetEventType,
		ProjectID: event.ProjectID, ProjectUID: event.ProjectUID,
		ProjectName: event.ProjectName, CreatedAt: event.CreatedAt,
	}
}

// EventDepartureProjectUID returns the authorized source of an issue move.
// Scoped streams use it to deliver a reset to readers of the project the issue left.
func EventDepartureProjectUID(ctx context.Context, event Event) string {
	if event.Type != "issue.moved" {
		return ""
	}
	var payload struct {
		FromProjectUID string `json:"from_project_uid"`
	}
	if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil || payload.FromProjectUID == "" {
		return ""
	}
	allowed, restricted := AuthorizedProjects(ctx)
	if !restricted {
		return ""
	}
	for _, uid := range allowed {
		if uid == payload.FromProjectUID {
			return uid
		}
	}
	return ""
}

// ProjectScopeResetEventInProject places a reset cursor in the project whose
// readers need it, while retaining none of the moved issue's identity.
func ProjectScopeResetEventInProject(event Event, project Project) Event {
	event.ProjectID = project.ID
	event.ProjectUID = project.UID
	event.ProjectName = project.Name
	return ProjectScopeResetEvent(event)
}

// ProjectScopeResetCursor is an identity-free fallback when the visible source
// project was removed before the move invalidation could be projected.
func ProjectScopeResetCursor(event Event) Event {
	return Event{ID: event.ID, Type: ProjectScopeResetEventType, CreatedAt: event.CreatedAt}
}
