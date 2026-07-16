package db

import (
	"encoding/json"
	"fmt"
)

// IssueFieldEdit is the backend-neutral result of comparing requested scalar
// edits with the current issue.
type IssueFieldEdit struct {
	TitleChanged bool
	BodyChanged  bool
	OwnerChanged bool
	Owner        *string
	Payload      string
}

// Changed reports whether at least one scalar field differs.
func (e IssueFieldEdit) Changed() bool {
	return e.TitleChanged || e.BodyChanged || e.OwnerChanged
}

// ContentChanged reports whether the edit changes embeddable issue content.
func (e IssueFieldEdit) ContentChanged() bool { return e.TitleChanged || e.BodyChanged }

// PlanIssueFieldEdit normalizes owner clearing and constructs the shared
// issue.updated payload without prescribing backend-specific SQL.
func PlanIssueFieldEdit(issue Issue, title, body, owner *string, updatedAt string) (IssueFieldEdit, error) {
	plan := IssueFieldEdit{
		TitleChanged: title != nil && *title != issue.Title,
		BodyChanged:  body != nil && *body != issue.Body,
	}
	if owner != nil {
		if *owner != "" {
			value := *owner
			plan.Owner = &value
		}
		plan.OwnerChanged = !equalOptionalString(issue.Owner, plan.Owner)
	}
	if !plan.Changed() {
		return plan, nil
	}
	payload := make(map[string]any)
	if plan.TitleChanged {
		payload["title"], payload["old_title"] = *title, issue.Title
	}
	if plan.BodyChanged {
		payload["body"] = *body
	}
	if plan.OwnerChanged {
		payload["owner"], payload["old_owner"] = plan.Owner, issue.Owner
	}
	payload["updated_at"] = updatedAt
	encoded, err := json.Marshal(payload)
	if err != nil {
		return IssueFieldEdit{}, fmt.Errorf("marshal issue.updated payload: %w", err)
	}
	plan.Payload = string(encoded)
	return plan, nil
}

// PriorityEventPayload constructs the shared event for a real priority
// transition. Callers must short-circuit equal old and new values first.
func PriorityEventPayload(old, next *int64, updatedAt string) (string, string, error) {
	if next != nil {
		payload := struct {
			Priority    int64  `json:"priority"`
			OldPriority *int64 `json:"old_priority,omitempty"`
			UpdatedAt   string `json:"updated_at"`
		}{Priority: *next, OldPriority: old, UpdatedAt: updatedAt}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", "", fmt.Errorf("marshal priority_set payload: %w", err)
		}
		return "issue.priority_set", string(encoded), nil
	}
	if old == nil {
		return "", "", fmt.Errorf("priority event: cannot clear a nil priority")
	}
	payload := struct {
		OldPriority int64  `json:"old_priority"`
		UpdatedAt   string `json:"updated_at"`
	}{OldPriority: *old, UpdatedAt: updatedAt}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal priority_cleared payload: %w", err)
	}
	return "issue.priority_cleared", string(encoded), nil
}

type linksChangedWirePayload struct {
	ParentSet            *string  `json:"parent_set,omitempty"`
	ParentSetUID         *string  `json:"parent_set_uid,omitempty"`
	ParentRemoved        *string  `json:"parent_removed,omitempty"`
	ParentRemovedUID     *string  `json:"parent_removed_uid,omitempty"`
	BlocksAdded          []string `json:"blocks_added,omitempty"`
	BlocksAddedUIDs      []string `json:"blocks_added_uids,omitempty"`
	BlocksRemoved        []string `json:"blocks_removed,omitempty"`
	BlocksRemovedUIDs    []string `json:"blocks_removed_uids,omitempty"`
	BlockedByAdded       []string `json:"blocked_by_added,omitempty"`
	BlockedByAddedUIDs   []string `json:"blocked_by_added_uids,omitempty"`
	BlockedByRemoved     []string `json:"blocked_by_removed,omitempty"`
	BlockedByRemovedUIDs []string `json:"blocked_by_removed_uids,omitempty"`
	RelatedAdded         []string `json:"related_added,omitempty"`
	RelatedAddedUIDs     []string `json:"related_added_uids,omitempty"`
	RelatedRemoved       []string `json:"related_removed,omitempty"`
	RelatedRemovedUIDs   []string `json:"related_removed_uids,omitempty"`
	UpdatedAt            string   `json:"updated_at"`
}

// LinksChangedPayload projects rich peer identities onto the stable event
// wire format shared by every storage backend.
func LinksChangedPayload(changes AtomicEditChanges, updatedAt string) ([]byte, error) {
	payload := linksChangedWirePayload{
		BlocksAdded:          peerField(changes.BlocksAdded, false),
		BlocksAddedUIDs:      peerField(changes.BlocksAdded, true),
		BlocksRemoved:        peerField(changes.BlocksRemoved, false),
		BlocksRemovedUIDs:    peerField(changes.BlocksRemoved, true),
		BlockedByAdded:       peerField(changes.BlockedByAdded, false),
		BlockedByAddedUIDs:   peerField(changes.BlockedByAdded, true),
		BlockedByRemoved:     peerField(changes.BlockedByRemoved, false),
		BlockedByRemovedUIDs: peerField(changes.BlockedByRemoved, true),
		RelatedAdded:         peerField(changes.RelatedAdded, false),
		RelatedAddedUIDs:     peerField(changes.RelatedAdded, true),
		RelatedRemoved:       peerField(changes.RelatedRemoved, false),
		RelatedRemovedUIDs:   peerField(changes.RelatedRemoved, true),
		UpdatedAt:            updatedAt,
	}
	if changes.ParentSet != nil {
		payload.ParentSet, payload.ParentSetUID = &changes.ParentSet.ShortID, &changes.ParentSet.UID
	}
	if changes.ParentRemoved != nil {
		payload.ParentRemoved, payload.ParentRemovedUID = &changes.ParentRemoved.ShortID, &changes.ParentRemoved.UID
	}
	return json.Marshal(payload)
}

func peerField(peers []PeerIdentity, uid bool) []string {
	if len(peers) == 0 {
		return nil
	}
	values := make([]string, len(peers))
	for index, peer := range peers {
		if uid {
			values[index] = peer.UID
		} else {
			values[index] = peer.ShortID
		}
	}
	return values
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
