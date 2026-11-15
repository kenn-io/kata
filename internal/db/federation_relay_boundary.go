package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/kata/internal/notification"
)

// RelayEventPeerProjects resolves the projects named by a source event.
// The lookup must use an unrestricted project context on the held transaction.
func RelayEventPeerProjects(event RemoteEvent, lookup *EventIssueLookup) ([]string, error) {
	projects := map[string]bool{}
	add := func(issue Issue, err error) (bool, error) {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if issue.ProjectUID != "" {
			projects[issue.ProjectUID] = true
		}
		return false, nil
	}
	issueUID := func(uid string) (bool, error) {
		if uid == "" {
			return false, nil
		}
		issue, err := lookup.IssueByUID(uid)
		return add(issue, err)
	}
	issueRef := func(ref string) (bool, error) {
		if ref == "" {
			return false, nil
		}
		issue, err := lookup.IssueByRef(event.ProjectUID, ref)
		return add(issue, err)
	}
	comment := func(uid, kind string) (bool, error) {
		if len(uid) != 26 || !ValidCommentReplyKind(kind) {
			return false, nil
		}
		issue, err := lookup.CommentIssueByUID(uid)
		return add(issue, err)
	}
	project := func(uid string) (bool, error) {
		if uid != "" {
			projects[uid] = true
		}
		return false, nil
	}
	if event.IssueUID != nil && *event.IssueUID != "" {
		if _, err := issueUID(*event.IssueUID); err != nil {
			return nil, err
		}
	}
	// Relay release checks reply and link targets that scoped readers handle elsewhere.
	switch event.Type {
	case "issue.commented":
		var payload struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && (payload.ReplyToUID != "" || payload.ReplyKind != "") {
			if _, err := comment(payload.ReplyToUID, payload.ReplyKind); err != nil {
				return nil, err
			}
		}
	case "issue.linked", "issue.unlinked":
		var payload map[string]jsontext.Value
		if json.Unmarshal(event.Payload, &payload) == nil {
			for _, key := range []string{"from_uid", "to_uid", "from_issue_uid", "to_issue_uid"} {
				if uid, ok := StringValue(payload[key]); ok && uid != "" {
					if _, err := issueUID(uid); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	_, err := walkEventProjectReferences(Event{ProjectUID: event.ProjectUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Payload: string(event.Payload)}, issueUID, issueRef, comment, project)
	if err != nil {
		return nil, err
	}
	_, err = notification.WalkEventPayload(jsontext.Value(event.Payload), event.Type, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notification.References(slot) {
			issue, err := lookup.CommentIssueByUID(uid)
			_, err = add(issue, err)
			if err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(projects))
	for uid := range projects {
		result = append(result, uid)
	}
	return result, nil
}

// AuthorizeRelayRelease checks every distinct peer before releasing a batch.
func AuthorizeRelayRelease(ctx context.Context, tx Transaction, lookup *EventIssueLookup, ownProjectUID string, events []RemoteEvent) error {
	seen := map[string]bool{ownProjectUID: true}
	for _, event := range events {
		projects, err := RelayEventPeerProjects(event, lookup)
		if err != nil {
			return err
		}
		for _, uid := range projects {
			if seen[uid] {
				continue
			}
			seen[uid] = true
			if err := CheckProjectAccessTransaction(ctx, tx, uid); err != nil {
				return err
			}
		}
	}
	return nil
}
