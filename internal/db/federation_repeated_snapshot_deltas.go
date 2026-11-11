package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"time"
)

// RepeatedSnapshotDelta is one ordinary event the hub writes in place of a
// repeated issue.snapshot. Actor is the event actor.
type RepeatedSnapshotDelta struct {
	Type    string
	Actor   string
	Payload jsontext.Value
}

// RepeatedSnapshotLookups answers the questions about other issues that one
// snapshot cannot.
type RepeatedSnapshotLookups struct {
	// IssueKnown reports whether the hub holds the issue or the same push
	// brings it.
	IssueKnown func(uid string) (bool, error)
	// HasParent reports whether the issue already has a parent on the hub.
	HasParent func(childUID string) (bool, error)
}

type repeatedSnapshotDoc struct {
	UID                 string             `json:"uid"`
	Title               string             `json:"title"`
	Body                string             `json:"body"`
	Owner               *string            `json:"owner"`
	AssignmentExpiresOn *string            `json:"assignment_expires_on"`
	Priority            *int64             `json:"priority"`
	Status              string             `json:"status"`
	ClosedReason        *string            `json:"closed_reason"`
	ClosedAt            *string            `json:"closed_at"`
	DeletedAt           *string            `json:"deleted_at"`
	Metadata            jsontext.Value     `json:"metadata"`
	Labels              []string           `json:"labels"`
	Links               []foldSnapshotLink `json:"links"`
	Comments            []struct {
		CommentUID string `json:"comment_uid"`
		Author     string `json:"author"`
		Teammate   string `json:"teammate,omitempty"`
		Body       string `json:"body"`
		CreatedAt  string `json:"created_at"`
		EditedAt   string `json:"edited_at"`
		ReplyToUID string `json:"reply_to_uid"`
		ReplyKind  string `json:"reply_kind"`
	} `json:"comments"`
	UpdatedAt string `json:"updated_at"`
}

// RepeatedSnapshotWins reports whether an adoption snapshot of an issue the
// hub project already holds is strictly newer than the hub's copy. The data's
// updated_at decides, compared as a time at the millisecond precision both
// stores keep. An equal, older, missing or unparseable time loses, and the hub
// then writes nothing for the snapshot.
func RepeatedSnapshotWins(incoming jsontext.Value, hubUpdatedAt time.Time) bool {
	var doc struct {
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(incoming, &doc); err != nil || doc.UpdatedAt == "" {
		return false
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, doc.UpdatedAt)
	if err != nil {
		return false
	}
	return updatedAt.Truncate(time.Millisecond).After(hubUpdatedAt.Truncate(time.Millisecond))
}

// RepeatedSnapshotDeltas turns a winning repeated snapshot into the ordinary
// events that bring the hub's copy to it. current is the hub's own snapshot of
// the issue and incoming is the pushed one.
//
// The hub stores these events and not the snapshot. A stored snapshot is read
// as the complete state of an issue by the move replay, the move history
// filter and the relay, and a repeated snapshot is not complete: comments
// merge by UID and links are only added. Ordinary events keep every snapshot
// in a hub's log complete.
//
// Fields, labels and metadata end up equal to the incoming copy. A comment the
// hub lacks is added with its author and created time; a known comment keeps
// both and takes the incoming text only with a later edit time. Links are only
// added, an import never replaces an issue's parent, and a link to an issue
// the hub does not hold is left out. The first event always carries the
// incoming updated_at, so the same copy pushed again compares equal.
func RepeatedSnapshotDeltas(actor string, current, incoming jsontext.Value, lookups RepeatedSnapshotLookups) ([]RepeatedSnapshotDelta, error) {
	var have, want repeatedSnapshotDoc
	if err := json.Unmarshal(current, &have); err != nil {
		return nil, fmt.Errorf("decode current issue snapshot: %w", err)
	}
	if err := json.Unmarshal(incoming, &want); err != nil {
		return nil, fmt.Errorf("%w: decode repeated issue snapshot: %w", ErrFederationIngestValidation, err)
	}
	var deltas []RepeatedSnapshotDelta
	add := func(eventType, eventActor string, payload map[string]any) error {
		raw, err := json.Marshal(payload, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode %s for repeated issue snapshot: %w", eventType, err)
		}
		deltas = append(deltas, RepeatedSnapshotDelta{Type: eventType, Actor: eventActor, Payload: raw})
		return nil
	}

	fields := map[string]any{"updated_at": want.UpdatedAt}
	if want.Title != have.Title {
		fields["title"] = want.Title
	}
	if want.Body != have.Body {
		fields["body"] = want.Body
	}
	// The fold clears the assignment expiry whenever it sets the owner, so the
	// two travel together.
	if !equalOptional(want.Owner, have.Owner) || !sameOptionalTimestamp(want.AssignmentExpiresOn, have.AssignmentExpiresOn) {
		fields["owner"] = want.Owner
		fields["assignment_expires_on"] = want.AssignmentExpiresOn
	}
	if !equalOptional(want.Priority, have.Priority) {
		fields["priority"] = want.Priority
	}
	if want.Status != "" && want.Status != have.Status {
		fields["status"] = want.Status
	}
	if !equalOptional(want.ClosedReason, have.ClosedReason) {
		fields["closed_reason"] = want.ClosedReason
	}
	if !sameOptionalTimestamp(want.ClosedAt, have.ClosedAt) {
		fields["closed_at"] = want.ClosedAt
	}
	if !sameOptionalTimestamp(want.DeletedAt, have.DeletedAt) {
		fields["deleted_at"] = want.DeletedAt
	}
	if err := add("issue.updated", actor, fields); err != nil {
		return nil, err
	}

	for _, label := range slices.Sorted(slices.Values(have.Labels)) {
		if !slices.Contains(want.Labels, label) {
			if err := add("issue.unlabeled", actor, map[string]any{"label": label, "updated_at": want.UpdatedAt}); err != nil {
				return nil, err
			}
		}
	}
	for _, label := range slices.Sorted(slices.Values(want.Labels)) {
		if !slices.Contains(have.Labels, label) {
			if err := add("issue.labeled", actor, map[string]any{"label": label, "updated_at": want.UpdatedAt}); err != nil {
				return nil, err
			}
		}
	}

	if diff := repeatedSnapshotMetadataDiff(have.Metadata, want.Metadata); len(diff) > 0 {
		if err := add("issue.metadata_updated", actor, map[string]any{"diff": diff, "updated_at": want.UpdatedAt}); err != nil {
			return nil, err
		}
	}

	known := map[string]string{}
	for _, comment := range have.Comments {
		known[comment.CommentUID] = comment.EditedAt
	}
	for _, comment := range want.Comments {
		if comment.CommentUID == "" {
			continue
		}
		editedAt, exists := known[comment.CommentUID]
		if !exists {
			payload := map[string]any{
				"comment_uid": comment.CommentUID, "author": comment.Author, "body": comment.Body, "created_at": comment.CreatedAt,
			}
			for key, value := range map[string]string{
				"teammate": comment.Teammate, "edited_at": comment.EditedAt,
				"reply_to_uid": comment.ReplyToUID, "reply_kind": comment.ReplyKind,
			} {
				if value != "" {
					payload[key] = value
				}
			}
			if err := add("issue.commented", actor, payload); err != nil {
				return nil, err
			}
			continue
		}
		if comment.EditedAt != "" && foldTimeBefore(editedAt, comment.EditedAt) {
			if err := add("issue.comment_edited", actor, map[string]any{
				"comment_uid": comment.CommentUID, "body": comment.Body, "edited_at": comment.EditedAt,
			}); err != nil {
				return nil, err
			}
		}
	}

	held := map[FoldLinkKey]bool{}
	parentOf := ""
	for _, link := range have.Links {
		key := snapshotLinkKey(have.UID, link)
		held[key] = true
		if key.Type == "parent" && key.FromUID == have.UID {
			parentOf = key.ToUID
		}
	}
	for _, link := range want.Links {
		if link.ToIssueUID == "" {
			continue
		}
		key := snapshotLinkKey(have.UID, link)
		if held[key] || (key.Type == "related" && held[FoldLinkKey{FromUID: key.ToUID, ToUID: key.FromUID, Type: key.Type}]) {
			continue
		}
		if lookups.IssueKnown != nil {
			exists, err := lookups.IssueKnown(link.ToIssueUID)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
		}
		if key.Type == "parent" {
			hasParent := parentOf != ""
			if key.FromUID != have.UID && lookups.HasParent != nil {
				var err error
				if hasParent, err = lookups.HasParent(key.FromUID); err != nil {
					return nil, err
				}
			}
			if hasParent {
				continue
			}
		}
		linkActor := link.Author
		if linkActor == "" {
			linkActor = actor
		}
		held[key] = true
		if err := add("issue.linked", linkActor, map[string]any{
			"type": key.Type, "from_uid": key.FromUID, "to_uid": key.ToUID, "updated_at": want.UpdatedAt,
		}); err != nil {
			return nil, err
		}
	}
	return deltas, nil
}

// snapshotLinkKey resolves one snapshot link entry to its stored direction.
func snapshotLinkKey(issueUID string, link foldSnapshotLink) FoldLinkKey {
	from, to := issueUID, link.ToIssueUID
	if link.Incoming && link.Type != "related" {
		from, to = to, from
	}
	return FoldLinkKey{FromUID: from, ToUID: to, Type: link.Type}
}

func repeatedSnapshotMetadataDiff(have, want jsontext.Value) map[string]map[string]jsontext.Value {
	current, target := parseRawObject(have), parseRawObject(want)
	diff := map[string]map[string]jsontext.Value{}
	for _, key := range slices.Sorted(maps.Keys(current)) {
		if _, kept := target[key]; !kept {
			diff[key] = map[string]jsontext.Value{"from": current[key], "to": jsontext.Value("null")}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(target)) {
		from, exists := current[key]
		if exists && string(canonicalJSON(from)) == string(canonicalJSON(target[key])) {
			continue
		}
		if !exists {
			from = jsontext.Value("null")
		}
		diff[key] = map[string]jsontext.Value{"from": from, "to": target[key]}
	}
	return diff
}

func equalOptional[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// sameOptionalTimestamp compares two optional timestamps as times, because the two
// stores and their snapshots write the same instant in different formats.
func sameOptionalTimestamp(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, leftErr := time.Parse(time.RFC3339Nano, *a)
	right, rightErr := time.Parse(time.RFC3339Nano, *b)
	if leftErr != nil || rightErr != nil {
		return *a == *b
	}
	return left.Equal(right)
}
