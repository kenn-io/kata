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
// The visit callback receives a project UID and whether the reference is a link.
// An empty UID identifies an unresolved link endpoint.
func RelayEventPeerProjects(event RemoteEvent, lookup *EventIssueLookup, visit func(string, bool) error) error {
	add := func(issue Issue, err error, link bool) (bool, error) {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if issue.ProjectUID != "" {
			return false, visit(issue.ProjectUID, link)
		}
		return false, nil
	}
	issueUID := func(uid string) (bool, error) {
		if uid == "" {
			return false, nil
		}
		issue, err := lookup.IssueByUID(uid)
		return add(issue, err, false)
	}
	linkUID := func(uid string) (bool, error) {
		if uid == "" {
			return false, nil
		}
		issue, err := lookup.IssueByUID(uid)
		if errors.Is(err, ErrNotFound) || (err == nil && issue.ProjectUID == "") {
			return false, visit("", true)
		}
		return add(issue, err, true)
	}
	issueRef := func(ref string) (bool, error) {
		if ref == "" {
			return false, nil
		}
		issue, err := lookup.IssueByRef(event.ProjectUID, ref)
		return add(issue, err, false)
	}
	comment := func(uid, kind string) (bool, error) {
		if len(uid) != 26 || !ValidCommentReplyKind(kind) {
			return false, nil
		}
		issue, err := lookup.CommentIssueByUID(uid)
		return add(issue, err, false)
	}
	project := func(uid string) (bool, error) {
		if uid != "" {
			return false, visit(uid, false)
		}
		return false, nil
	}
	if event.IssueUID != nil && *event.IssueUID != "" {
		if _, err := issueUID(*event.IssueUID); err != nil {
			return err
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
				return err
			}
		}
	}
	_, err := walkEventProjectReferences(Event{ProjectUID: event.ProjectUID, RelatedIssueUID: event.RelatedIssueUID, Type: event.Type, Payload: string(event.Payload)}, issueUID, linkUID, issueRef, comment, project)
	if err != nil {
		return err
	}
	snapshot := FederationMoveSnapshotEvent(event)
	_, err = notification.WalkEventPayload(snapshot.Payload, snapshot.Type, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notification.References(slot) {
			issue, err := lookup.CommentIssueByUID(uid)
			_, err = add(issue, err, false)
			if err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// AuthorizeRelayRelease checks every distinct peer before releasing a batch.
func AuthorizeRelayRelease(ctx context.Context, tx Transaction, lookup *EventIssueLookup, ownProjectUID string, events []RemoteEvent) error {
	_, err := AuthorizeRelayReleasePrefix(ctx, tx, lookup, ownProjectUID, events)
	return err
}

// AuthorizeRelayReleasePrefix returns the first event that cannot be released.
func AuthorizeRelayReleasePrefix(ctx context.Context, tx Transaction, lookup *EventIssueLookup, ownProjectUID string, events []RemoteEvent) (int, error) {
	seen := map[string]bool{ownProjectUID: true}
	visit := func(uid string, link bool) error {
		if link && uid != ownProjectUID {
			if err := RequireEventFeatures(RelayPeerEventFeatures(ctx), RelayCrossLinksEventFeature); err != nil {
				return err
			}
		}
		if uid == "" {
			return nil
		}
		if seen[uid] {
			return nil
		}
		seen[uid] = true
		return CheckProjectAccessTransaction(ctx, tx, uid)
	}
	for i, event := range events {
		if err := RelayEventPeerProjects(event, lookup, visit); err != nil {
			return i, err
		}
	}
	return len(events), nil
}
