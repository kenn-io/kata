package db

import (
	"context"
	"encoding/json/jsontext"
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
	add := func(issue Issue, err error, required bool) (bool, error) {
		if errors.Is(err, ErrNotFound) {
			return required, nil
		}
		if err != nil {
			return false, err
		}
		projects[issue.ProjectUID] = true
		return false, nil
	}
	issueUID := func(uid string) (bool, error) { issue, err := lookup.IssueByUID(uid); return add(issue, err, true) }
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
		issue, err := lookup.IssueByRef(event.ProjectUID, ref)
		return add(issue, err, false)
	}
	comment := func(uid, kind string) (bool, error) {
		if len(uid) != 26 || !ValidCommentReplyKind(kind) {
			return true, nil
		}
		issue, err := lookup.CommentIssueByUID(uid)
		return add(issue, err, true)
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
	unprovable, err := walkEventProjectReferences(Event{ProjectUID: event.ProjectUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Payload: string(event.Payload)}, issueUID, issueRef, comment, project, true)
	if err != nil {
		return nil, err
	}
	if unprovable {
		return nil, ErrRelayReferenceUnprovable
	}
	_, err = notification.WalkEventPayload(jsontext.Value(event.Payload), event.Type, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notification.References(slot) {
			issue, err := lookup.CommentIssueByUID(uid)
			unprovable, err := add(issue, err, true)
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
