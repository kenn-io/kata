package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/notification"
)

// Notification references can appear in snapshots and in both sides of an
// event diff. Project them at the common response boundary so new read routes
// cannot accidentally bypass the same authorization policy.
func scopedNotificationTransformer(store db.Storage) func(huma.Context, string, any) (any, error) {
	return func(hctx huma.Context, _ string, value any) (any, error) {
		ctx := hctx.Context()
		if issueScopeFromContext(ctx) == nil {
			return value, nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		projected, err := projectNotificationJSON(ctx, store, raw)
		if err != nil {
			return nil, err
		}
		return projected, nil
	}
}

func projectNotificationJSON(ctx context.Context, store db.Storage, raw jsontext.Value) (jsontext.Value, error) {
	if issueScopeFromContext(ctx) == nil {
		return raw, nil
	}
	uids := map[string]bool{}
	_, err := walkNotificationJSON(raw, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notificationReferences(slot) {
			uids[uid] = true
		}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if len(uids) == 0 {
		return raw, nil
	}
	targets := make([]string, 0, len(uids))
	for uid := range uids {
		targets = append(targets, uid)
	}
	ids, err := store.CommentIssueIDsByUIDs(ctx, targets)
	if err != nil {
		return nil, err
	}
	allowed, _, err := issueScopedAllowedIDSet(ctx, store)
	if err != nil {
		return nil, err
	}
	return walkNotificationJSON(raw, func(slot jsontext.Value) (bool, error) {
		for _, uid := range notificationReferences(slot) {
			id, ok := ids[uid]
			if !ok {
				return false, nil
			}
			if _, ok := allowed[id]; !ok {
				return false, nil
			}
			db.RecordIssueScopeTarget(ctx, id)
		}
		return true, nil
	})
}

func notificationReferences(raw jsontext.Value) []string {
	var object map[string]jsontext.Value
	if json.Unmarshal(raw, &object) != nil {
		return nil
	}
	var out []string
	if re, ok := object["re"]; ok {
		var uid string
		if json.Unmarshal(re, &uid) != nil || uid == "" {
			uid = "<invalid>"
		}
		out = append(out, uid)
	}
	for _, key := range []string{"from", "to"} {
		if len(object[key]) > 0 && object[key][0] == '{' {
			out = append(out, notificationReferences(object[key])...)
		}
	}
	return out
}

// Only event payload and metadata strings are decoded as JSON. Bodies and
// other user strings remain opaque, including strings that resemble JSON.
func walkNotificationJSON(raw jsontext.Value, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw, nil
	}
	switch raw[0] {
	case '{':
		var object map[string]jsontext.Value
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		var eventType string
		_ = json.Unmarshal(object["type"], &eventType)
		for key, value := range object {
			var err error
			switch key {
			case "metadata":
				value, err = walkNotificationMetadata(value, keep)
			case "payload":
				value, err = walkNotificationEventPayload(value, eventType, keep)
			default:
				value, err = walkNotificationJSON(value, keep)
			}
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		return json.Marshal(object)
	case '[':
		var items []jsontext.Value
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i, v := range items {
			var err error
			items[i], err = walkNotificationJSON(v, keep)
			if err != nil {
				return nil, err
			}
		}
		return json.Marshal(items)
	default:
		return raw, nil
	}
}

func walkNotificationMetadata(raw jsontext.Value, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw, nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, err
		}
		if !jsontext.Value(encoded).IsValid() {
			return raw, nil
		}
		projected, err := walkNotificationMetadata(jsontext.Value(encoded), keep)
		if err != nil {
			return nil, err
		}
		return json.Marshal(string(projected))
	}
	if raw[0] != '{' {
		return raw, nil
	}
	var slots map[string]jsontext.Value
	if err := json.Unmarshal(raw, &slots); err != nil {
		return nil, err
	}
	return walkNotificationSlots(slots, keep)
}

// Notification slots are top-level keys in issue metadata and in an
// issue.metadata_updated diff. Values below any other key are opaque metadata.
func walkNotificationSlots(slots map[string]jsontext.Value, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	for key, value := range slots {
		if !strings.HasPrefix(key, notification.KeyPrefix) {
			continue
		}
		ok, err := keep(value)
		if err != nil {
			return nil, err
		}
		if !ok {
			delete(slots, key)
		}
	}
	return json.Marshal(slots)
}

func walkNotificationEventPayload(raw jsontext.Value, eventType string, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	if eventType != "issue.metadata_updated" && eventType != "issue.created" && eventType != "issue.snapshot" {
		return raw, nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw, nil
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, err
		}
		if !jsontext.Value(encoded).IsValid() {
			return raw, nil
		}
		projected, err := walkNotificationEventPayload(jsontext.Value(encoded), eventType, keep)
		if err != nil {
			return nil, err
		}
		return json.Marshal(string(projected))
	}
	if raw[0] != '{' {
		return raw, nil
	}
	var payload map[string]jsontext.Value
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	field := "metadata"
	if eventType == "issue.metadata_updated" {
		field = "diff"
	}
	value, ok := payload[field]
	if !ok || len(value) == 0 || value[0] != '{' {
		return raw, nil
	}
	var slots map[string]jsontext.Value
	if err := json.Unmarshal(value, &slots); err != nil {
		return nil, err
	}
	projected, err := walkNotificationSlots(slots, keep)
	if err != nil {
		return nil, err
	}
	payload[field] = projected
	return json.Marshal(payload)
}

// Stream frames recheck notification targets after page projection, immediately
// before release. Membership read during projection is only a snapshot.
func notificationEventStillVisible(ctx context.Context, store db.Storage, event db.Event) (bool, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return true, nil
	}
	var uids []string
	_, err := walkNotificationEventPayload(jsontext.Value(event.Payload), event.Type, func(slot jsontext.Value) (bool, error) {
		uids = append(uids, notificationReferences(slot)...)
		return true, nil
	})
	if err != nil {
		return false, err
	}
	if len(uids) == 0 {
		return true, nil
	}
	targets, err := store.CommentIssueIDsByUIDs(ctx, uids)
	if err != nil {
		return false, err
	}
	var ids []int64
	seen := make(map[int64]bool)
	for _, uid := range uids {
		id, ok := targets[uid]
		if !ok {
			return false, nil
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return store.IssueInScope(ctx, *scope, ids...)
}
