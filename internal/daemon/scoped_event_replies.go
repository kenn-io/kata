package daemon

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"

	"go.kenn.io/kata/internal/db"
)

// projectIssueScopedEvents projects a batch of issue events and restores
// comment reply endpoints only when their current target comment belongs to
// the caller's active issue subtree. The context-free event projection omits
// reply fields because the event envelope may have lost its related issue
// identity during import, purge, or federation replay.
func projectIssueScopedEvents(
	ctx context.Context,
	store db.Storage,
	events []db.Event,
	allowed map[int64]struct{},
	projectUID string,
) ([]db.Event, []bool, error) {
	type replyProjection struct {
		targetUID       string
		originalPayload string
	}

	projected := make([]db.Event, len(events))
	visible := make([]bool, len(events))
	replyByIndex := make([]replyProjection, len(events))
	uniqueReplyUIDs := make([]string, 0)
	seenReplyUIDs := make(map[string]struct{})

	for index, event := range events {
		projection, ok := projectIssueScopedEvent(event, allowed, projectUID)
		if !ok {
			continue
		}
		projected[index] = projection
		visible[index] = true
		if event.Type != "issue.commented" {
			continue
		}
		targetUID, ok := scopedEventReplyTarget(event.Payload)
		if !ok {
			continue
		}
		replyByIndex[index] = replyProjection{targetUID: targetUID, originalPayload: event.Payload}
		if _, exists := seenReplyUIDs[targetUID]; exists {
			continue
		}
		seenReplyUIDs[targetUID] = struct{}{}
		uniqueReplyUIDs = append(uniqueReplyUIDs, targetUID)
	}
	if len(uniqueReplyUIDs) == 0 {
		return projected, visible, nil
	}

	targetIssueIDs, err := store.CommentIssueIDsByUIDs(ctx, uniqueReplyUIDs)
	if err != nil {
		return nil, nil, err
	}
	for index, reply := range replyByIndex {
		if reply.targetUID == "" {
			continue
		}
		targetIssueID, found := targetIssueIDs[reply.targetUID]
		if !found {
			continue
		}
		if _, ok := allowed[targetIssueID]; !ok {
			continue
		}
		payload, err := restoreScopedEventReplyFields(projected[index].Payload, reply.originalPayload)
		if err != nil {
			return nil, nil, err
		}
		if payload != projected[index].Payload {
			// Poll, snapshot, report, and mutation responses are buffered by
			// withScopedPrincipalRevalidation. Register every restored endpoint
			// so the response guard rechecks subtree membership after projection.
			db.RecordIssueScopeTarget(ctx, targetIssueID)
		}
		projected[index].Payload = payload
	}
	return projected, visible, nil
}

func scopedEventReplyTarget(payload string) (string, bool) {
	var fields struct {
		ReplyToUID string `json:"reply_to_uid"`
	}
	if json.Unmarshal([]byte(payload), &fields) != nil || len(fields.ReplyToUID) != 26 {
		return "", false
	}
	return fields.ReplyToUID, true
}

func restoreScopedEventReplyFields(projectedPayload, originalPayload string) (string, error) {
	var original map[string]jsontext.Value
	if err := json.Unmarshal([]byte(originalPayload), &original); err != nil {
		return projectedPayload, nil
	}
	var replyKind string
	if raw, ok := original["reply_kind"]; !ok || json.Unmarshal(raw, &replyKind) != nil || !db.ValidCommentReplyKind(replyKind) {
		return projectedPayload, nil
	}
	var projected map[string]jsontext.Value
	if err := json.Unmarshal([]byte(projectedPayload), &projected); err != nil {
		return "", err
	}
	if projected == nil {
		projected = make(map[string]jsontext.Value)
	}
	projected["reply_to_uid"] = original["reply_to_uid"]
	projected["reply_kind"] = original["reply_kind"]
	encoded, err := json.Marshal(projected)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
