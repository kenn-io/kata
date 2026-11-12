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
// repeated issue.snapshot. Actor is the event actor. RelatedIssueUID names the
// other end of a link event, as ordinary link events do.
type RepeatedSnapshotDelta struct {
	Type            string
	Actor           string
	Payload         jsontext.Value
	RelatedIssueUID string
	// At is the data's own time for the change, when it has one that is
	// earlier than the copy's updated_at. A comment edit has its edit time.
	At string
}

// Clock places the delta at position index among a winning copy's events,
// which start at base. A delta with its own earlier time sits there instead,
// so a later change to the same data that the copy never saw still replays
// after it.
func (d RepeatedSnapshotDelta) Clock(base EventHLCTimestamp, index int) EventHLCTimestamp {
	if at, err := time.Parse(time.RFC3339Nano, d.At); err == nil && at.UnixMilli() < base.PhysicalMS {
		return EventHLCTimestamp{PhysicalMS: at.UnixMilli(), Counter: int64(index)}
	}
	return EventHLCTimestamp{PhysicalMS: base.PhysicalMS, Counter: base.Counter + int64(index)}
}

// RepeatedSnapshotLookups answers the questions about other issues that one
// snapshot cannot. One value serves every repeated snapshot of a push, so a
// link or parent added for one snapshot is seen by the next.
type RepeatedSnapshotLookups struct {
	// IssueKnown reports whether the hub holds the issue or the same push
	// brings it.
	IssueKnown func(uid string) (bool, error)
	// ParentOf returns the issue's parent on the hub, or "" when it has none.
	ParentOf func(childUID string) (string, error)

	parents map[string]string
	links   map[FoldLinkKey]bool
}

// SetParent records a parent the same push establishes outside the repeated
// snapshots, such as one a fresh snapshot carries.
func (l *RepeatedSnapshotLookups) SetParent(childUID, parentUID string) {
	if l.parents == nil {
		l.parents = map[string]string{}
	}
	if _, exists := l.parents[childUID]; !exists {
		l.parents[childUID] = parentUID
	}
}

func (l *RepeatedSnapshotLookups) parentOf(childUID string) (string, error) {
	if parent, ok := l.parents[childUID]; ok {
		return parent, nil
	}
	if l.ParentOf == nil {
		return "", nil
	}
	return l.ParentOf(childUID)
}

// acceptsParent reports whether childUID may take parentUID: the child has no
// parent yet, and the new link closes no cycle.
func (l *RepeatedSnapshotLookups) acceptsParent(childUID, parentUID string) (bool, error) {
	current, err := l.parentOf(childUID)
	if err != nil || current != "" {
		return false, err
	}
	ancestor := parentUID
	for range MaxParentDepth {
		if ancestor == childUID {
			return false, nil
		}
		if ancestor, err = l.parentOf(ancestor); err != nil {
			return false, err
		}
		if ancestor == "" {
			return true, nil
		}
	}
	return false, nil
}

// repeatedSnapshotLink is one link entry of an issue.snapshot payload.
type repeatedSnapshotLink struct {
	Type       string `json:"type"`
	ToIssueUID string `json:"to_issue_uid"`
	Incoming   bool   `json:"incoming"`
	Author     string `json:"author"`
}

type repeatedSnapshotDoc struct {
	UID                 string                 `json:"uid"`
	Title               string                 `json:"title"`
	Body                string                 `json:"body"`
	Owner               *string                `json:"owner"`
	AssignmentExpiresOn *string                `json:"assignment_expires_on"`
	Priority            *int64                 `json:"priority"`
	Status              string                 `json:"status"`
	ClosedReason        *string                `json:"closed_reason"`
	ClosedAt            *string                `json:"closed_at"`
	DeletedAt           *string                `json:"deleted_at"`
	Metadata            jsontext.Value         `json:"metadata"`
	Labels              []string               `json:"labels"`
	Links               []repeatedSnapshotLink `json:"links"`
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
// Fields, labels and metadata end up equal to the incoming copy, and the
// events restate the values the two copies share. A comment the
// hub lacks is added with its author and created time; a known comment keeps
// both and takes the incoming text only with a later edit time. Links are only
// added. An import never replaces an issue's parent or closes a parent cycle,
// and a link to an issue the hub does not hold is left out. The first event always carries the
// incoming updated_at, so the same copy pushed again compares equal.
func RepeatedSnapshotDeltas(actor string, current, incoming jsontext.Value, lookups *RepeatedSnapshotLookups) ([]RepeatedSnapshotDelta, error) {
	if lookups == nil {
		lookups = &RepeatedSnapshotLookups{}
	}
	var have, want repeatedSnapshotDoc
	if err := json.Unmarshal(current, &have); err != nil {
		return nil, fmt.Errorf("decode current issue snapshot: %w", err)
	}
	if err := json.Unmarshal(incoming, &want); err != nil {
		return nil, fmt.Errorf("%w: decode repeated issue snapshot: %w", ErrFederationIngestValidation, err)
	}
	var deltas []RepeatedSnapshotDelta
	emit := func(delta RepeatedSnapshotDelta, payload map[string]any) error {
		raw, err := json.Marshal(payload, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode %s for repeated issue snapshot: %w", delta.Type, err)
		}
		delta.Payload = raw
		deltas = append(deltas, delta)
		return nil
	}
	add := func(eventType, eventActor string, payload map[string]any) error {
		return emit(RepeatedSnapshotDelta{Type: eventType, Actor: eventActor}, payload)
	}

	// The events restate the whole copy, including the values it shares with
	// the hub. An older edit can reach the hub after the import and replays
	// before these events, so a value they left out would fall back to it.
	fields := map[string]any{
		"updated_at": want.UpdatedAt, "title": want.Title, "body": want.Body,
		// The fold clears the assignment expiry whenever it sets the owner, so
		// the two travel together.
		"owner": want.Owner, "assignment_expires_on": want.AssignmentExpiresOn,
		"priority": want.Priority, "closed_reason": want.ClosedReason,
		"closed_at": want.ClosedAt, "deleted_at": want.DeletedAt,
	}
	if want.Status != "" {
		fields["status"] = want.Status
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
		if err := add("issue.labeled", actor, map[string]any{"label": label, "updated_at": want.UpdatedAt}); err != nil {
			return nil, err
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
			// The comment's creation can arrive again later from another
			// home, with earlier edits. The fold keeps the first creation it
			// replays, so a separate edit event carries the copy's text.
			if comment.EditedAt != "" {
				if err := emit(RepeatedSnapshotDelta{Type: "issue.comment_edited", Actor: actor, At: comment.EditedAt}, map[string]any{
					"comment_uid": comment.CommentUID, "body": comment.Body, "edited_at": comment.EditedAt,
				}); err != nil {
					return nil, err
				}
			}
			continue
		}
		if comment.EditedAt != "" && timestampBefore(editedAt, comment.EditedAt) {
			if err := emit(RepeatedSnapshotDelta{Type: "issue.comment_edited", Actor: actor, At: comment.EditedAt}, map[string]any{
				"comment_uid": comment.CommentUID, "body": comment.Body, "edited_at": comment.EditedAt,
			}); err != nil {
				return nil, err
			}
		}
	}

	held := map[FoldLinkKey]bool{}
	for _, link := range have.Links {
		key := snapshotLinkKey(have.UID, link)
		held[key] = true
		if key.Type == "parent" {
			lookups.SetParent(key.FromUID, key.ToUID)
		}
	}
	if lookups.links == nil {
		lookups.links = map[FoldLinkKey]bool{}
	}
	for _, link := range want.Links {
		if link.ToIssueUID == "" {
			continue
		}
		key := snapshotLinkKey(have.UID, link)
		reverse := FoldLinkKey{FromUID: key.ToUID, ToUID: key.FromUID, Type: key.Type}
		if held[key] || lookups.links[key] || (key.Type == "related" && (held[reverse] || lookups.links[reverse])) {
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
			accepts, err := lookups.acceptsParent(key.FromUID, key.ToUID)
			if err != nil {
				return nil, err
			}
			if !accepts {
				continue
			}
			lookups.SetParent(key.FromUID, key.ToUID)
		}
		linkActor := link.Author
		if linkActor == "" {
			linkActor = actor
		}
		lookups.links[key] = true
		if err := emit(RepeatedSnapshotDelta{Type: "issue.linked", Actor: linkActor, RelatedIssueUID: link.ToIssueUID}, map[string]any{
			"type": key.Type, "from_uid": key.FromUID, "to_uid": key.ToUID, "updated_at": want.UpdatedAt,
		}); err != nil {
			return nil, err
		}
	}
	return deltas, nil
}

// snapshotLinkKey resolves one snapshot link entry to its stored direction.
func snapshotLinkKey(issueUID string, link repeatedSnapshotLink) FoldLinkKey {
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
		// The hub owns the recurring-work keys. A copy from another home never
		// carries them, so their absence is not a removal.
		if recurringMetadataKey(key) {
			continue
		}
		if _, kept := target[key]; !kept {
			diff[key] = map[string]jsontext.Value{"from": current[key], "to": jsontext.Value("null")}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(target)) {
		if recurringMetadataKey(key) {
			continue
		}
		// A key the copy shares with the hub is restated too, for the same
		// reason as an unchanged field.
		from, exists := current[key]
		if !exists {
			from = jsontext.Value("null")
		}
		diff[key] = map[string]jsontext.Value{"from": from, "to": target[key]}
	}
	return diff
}

// timestampBefore reports whether timestamp a is earlier than b. An empty a is
// earlier than any b, and nothing is earlier than an empty b.
func timestampBefore(a, b string) bool {
	if b == "" {
		return false
	}
	if a == "" {
		return true
	}
	left, leftErr := time.Parse(time.RFC3339Nano, a)
	right, rightErr := time.Parse(time.RFC3339Nano, b)
	if leftErr != nil || rightErr != nil {
		return a < b
	}
	return left.Before(right)
}
