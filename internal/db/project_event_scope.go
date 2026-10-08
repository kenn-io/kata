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

// EventRequiresProjectScopeReset detects structured references that cannot be
// released to a project-scoped reader. The lookup must honor ctx's authorized
// project boundary so inaccessible issue identities resolve as not found.
func EventRequiresProjectScopeReset(
	ctx context.Context,
	event Event,
	issueByUID func(string) (Issue, error),
) (bool, error) {
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
			ParentUID     *string `json:"parent_uid"`
			ParentShortID *string `json:"parent_short_id"`
		}
		if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
			return true, nil
		}
		if payload.ParentUID == nil || *payload.ParentUID == "" {
			return payload.ParentShortID != nil && *payload.ParentShortID != "", nil
		}
		return issueIsPrivate(*payload.ParentUID)
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
