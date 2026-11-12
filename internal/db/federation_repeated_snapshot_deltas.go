package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"
)

// RepeatedImportEventType is the event a hub writes for a winning repeated
// adoption snapshot. Its payload is the whole issue as the copy had it: every
// field, the label set, the metadata and the comments, with the copy's
// updated_at.
//
// The fold applies it only when that updated_at is later than the issue's at
// that point of the replay, which is the comparison the hub made when the copy
// arrived. The event therefore gives the same result wherever it sits among
// the other events, and it takes the hub's ordinary clock. A reader needs
// RepeatedAdoptionEventFeature to replay it.
const RepeatedImportEventType = "issue.restated"

// RepeatedSnapshotDelta is one event the hub writes in place of a repeated
// issue.snapshot. Actor is the event actor. RelatedIssueUID names the other
// end of a link event, as ordinary link events do.
type RepeatedSnapshotDelta struct {
	Type            string
	Actor           string
	Payload         jsontext.Value
	RelatedIssueUID string
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
	UID                 string                  `json:"uid"`
	Title               string                  `json:"title"`
	Body                string                  `json:"body"`
	Owner               *string                 `json:"owner"`
	AssignmentExpiresOn *string                 `json:"assignment_expires_on"`
	Priority            *int64                  `json:"priority"`
	Status              string                  `json:"status"`
	ClosedReason        *string                 `json:"closed_reason"`
	ClosedAt            *string                 `json:"closed_at"`
	DeletedAt           *string                 `json:"deleted_at"`
	Metadata            jsontext.Value          `json:"metadata"`
	Labels              []string                `json:"labels"`
	Links               []repeatedSnapshotLink  `json:"links"`
	Comments            []repeatedImportComment `json:"comments"`
	UpdatedAt           string                  `json:"updated_at"`
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

// repeatedImportComment is one comment of a RepeatedImportEventType payload.
type repeatedImportComment struct {
	CommentUID string `json:"comment_uid"`
	Author     string `json:"author"`
	Teammate   string `json:"teammate,omitempty"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
	EditedAt   string `json:"edited_at,omitempty"`
	ReplyToUID string `json:"reply_to_uid,omitempty"`
	ReplyKind  string `json:"reply_kind,omitempty"`
}

// repeatedImportPayload is the payload of a RepeatedImportEventType event.
type repeatedImportPayload struct {
	Title               string                    `json:"title"`
	Body                string                    `json:"body"`
	Owner               *string                   `json:"owner"`
	AssignmentExpiresOn *string                   `json:"assignment_expires_on"`
	Priority            *int64                    `json:"priority"`
	Status              string                    `json:"status,omitempty"`
	ClosedReason        *string                   `json:"closed_reason"`
	ClosedAt            *string                   `json:"closed_at"`
	DeletedAt           *string                   `json:"deleted_at"`
	Metadata            map[string]jsontext.Value `json:"metadata"`
	Labels              []string                  `json:"labels"`
	Comments            []repeatedImportComment   `json:"comments"`
	UpdatedAt           string                    `json:"updated_at"`
}

// RepeatedSnapshotDeltas turns a winning repeated snapshot into the events the
// hub writes for it. current is the hub's own snapshot of the issue and
// incoming is the pushed one.
//
// The hub stores these events and not the snapshot. A stored snapshot is read
// as the complete state of an issue by the move replay, the move history
// filter and the relay, and a repeated snapshot is not complete: the issue
// keeps its author, comments merge by UID and links are only added.
//
// The first event is a RepeatedImportEventType with the whole copy. Fields,
// labels and metadata become the copy's, and the recurrence keys the hub's
// scheduler owns stay. A comment the hub lacks is added with its author and
// created time; a known comment keeps both and takes the copy's text only
// with a later edit time. An issue.linked event follows for each link the hub
// lacks. An import never replaces an issue's parent or closes a parent cycle,
// and a link to an issue the hub does not hold is left out.
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
	emit := func(delta RepeatedSnapshotDelta, payload any) error {
		raw, err := json.Marshal(payload, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("encode %s for repeated issue snapshot: %w", delta.Type, err)
		}
		delta.Payload = raw
		deltas = append(deltas, delta)
		return nil
	}

	restated := repeatedImportPayload{
		Title: want.Title, Body: want.Body, Owner: want.Owner, AssignmentExpiresOn: want.AssignmentExpiresOn,
		Priority: want.Priority, Status: want.Status, ClosedReason: want.ClosedReason, ClosedAt: want.ClosedAt,
		DeletedAt: want.DeletedAt, Metadata: map[string]jsontext.Value{},
		Labels: slices.Sorted(slices.Values(want.Labels)), Comments: []repeatedImportComment{}, UpdatedAt: want.UpdatedAt,
	}
	if restated.Labels == nil {
		restated.Labels = []string{}
	}
	for key, value := range parseRawObject(want.Metadata) {
		if !recurringMetadataKey(key) {
			restated.Metadata[key] = value
		}
	}
	for _, comment := range want.Comments {
		if comment.CommentUID != "" {
			restated.Comments = append(restated.Comments, comment)
		}
	}
	if err := emit(RepeatedSnapshotDelta{Type: RepeatedImportEventType, Actor: actor}, restated); err != nil {
		return nil, err
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

// repeatedCopyIsNewer reports whether a copy's updated_at is later than an
// issue's, at the millisecond precision both stores keep. An issue with no
// time yet is older than any copy, and a copy with no readable time is never
// newer.
func repeatedCopyIsNewer(issueUpdatedAt, copyUpdatedAt string) bool {
	copied, err := time.Parse(time.RFC3339Nano, copyUpdatedAt)
	if err != nil {
		return false
	}
	if issueUpdatedAt == "" {
		return true
	}
	held, err := time.Parse(time.RFC3339Nano, issueUpdatedAt)
	if err != nil {
		return copyUpdatedAt > issueUpdatedAt
	}
	return copied.Truncate(time.Millisecond).After(held.Truncate(time.Millisecond))
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
