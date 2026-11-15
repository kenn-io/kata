package db

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"

	"go.kenn.io/kata/internal/notification"
	"go.kenn.io/kata/internal/shortid"
)

// ErrRelayReferenceUnprovable means a relay reference has no provable owner.
var ErrRelayReferenceUnprovable = errors.New("relay reference owner cannot be proved")

// RelayEventPeerProjects resolves the projects named by a source event.
// The lookup must use an unrestricted project context on the held transaction.
func RelayEventPeerProjects(event RemoteEvent, lookup *EventIssueLookup) ([]string, error) {
	projects := map[string]bool{}
	add := func(issue Issue, err error) (bool, error) {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		projects[issue.ProjectUID] = true
		return false, nil
	}
	issueUID := func(uid string) (bool, error) { issue, err := lookup.IssueByUID(uid); return add(issue, err) }
	issueRef := func(ref string) (bool, error) {
		if parsed, err := shortid.Parse(ref); err == nil {
			if parsed.ULID != "" {
				return issueUID(parsed.ULID)
			}
			if parsed.Project != "" && parsed.Project == event.ProjectName {
				projects[event.ProjectUID] = true
				return false, nil
			}
		}
		if strings.Contains(ref, "#") {
			return true, nil
		}
		projects[event.ProjectUID] = true
		return false, nil
	}
	comment := func(uid, kind string) (bool, error) {
		if len(uid) != 26 || !ValidCommentReplyKind(kind) {
			return true, nil
		}
		issue, err := lookup.CommentIssueByUID(uid)
		return add(issue, err)
	}
	project := func(uid string) (bool, error) { projects[uid] = true; return false, nil }
	if event.IssueUID != nil {
		unprovable, err := issueUID(*event.IssueUID)
		if err != nil {
			return nil, err
		}
		if unprovable {
			return nil, ErrRelayReferenceUnprovable
		}
	}
	scopedEvent := event
	// Relay release checks reply and link targets that scoped readers handle elsewhere.
	switch event.Type {
	case "issue.commented":
		var payload struct {
			ReplyToUID string `json:"reply_to_uid"`
			ReplyKind  string `json:"reply_kind"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, ErrRelayReferenceUnprovable
		}
		if payload.ReplyToUID != "" || payload.ReplyKind != "" {
			unprovable, err := comment(payload.ReplyToUID, payload.ReplyKind)
			if err != nil {
				return nil, err
			}
			if unprovable {
				return nil, ErrRelayReferenceUnprovable
			}
		}
	case "issue.linked", "issue.unlinked":
		var payload map[string]jsontext.Value
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, ErrRelayReferenceUnprovable
		}
		for _, key := range []string{"from_uid", "to_uid", "from_issue_uid", "to_issue_uid"} {
			if raw, found := payload[key]; found {
				uid, ok := StringValue(raw)
				if !ok || uid == "" {
					return nil, ErrRelayReferenceUnprovable
				}
				unprovable, err := issueUID(uid)
				if err != nil {
					return nil, err
				}
				if unprovable {
					return nil, ErrRelayReferenceUnprovable
				}
			}
		}
	case "issue.closed":
		// Legacy short-only parents refer to this event's own project.
		var payload map[string]jsontext.Value
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, ErrRelayReferenceUnprovable
		}
		parentUID, _ := StringValue(payload["parent_uid"])
		parentShortID, _ := StringValue(payload["parent_short_id"])
		if parentUID == "" && parentShortID != "" {
			delete(payload, "parent_short_id")
			encoded, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			scopedEvent.Payload = encoded
		}
	}
	unprovable, err := walkEventProjectReferences(Event{ProjectUID: scopedEvent.ProjectUID, RelatedIssueUID: scopedEvent.RelatedIssueUID, Type: scopedEvent.Type, Payload: string(scopedEvent.Payload)}, issueUID, issueRef, comment, project)
	if err != nil {
		return nil, err
	}
	if unprovable {
		return nil, ErrRelayReferenceUnprovable
	}
	_, err = notification.WalkEventPayload(jsontext.Value(event.Payload), event.Type, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notification.References(slot) {
			issue, err := lookup.CommentIssueByUID(uid)
			unprovable, err := add(issue, err)
			if err != nil {
				return false, err
			}
			if unprovable {
				return false, ErrRelayReferenceUnprovable
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
