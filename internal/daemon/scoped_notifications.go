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
		for key, value := range object {
			if strings.HasPrefix(key, notification.KeyPrefix) {
				ok, err := keep(value)
				if err != nil {
					return nil, err
				}
				if !ok {
					delete(object, key)
				}
				continue
			}
			if (key == "payload" || key == "metadata") && len(value) > 0 && value[0] == '"' {
				var encoded string
				if err := json.Unmarshal(value, &encoded); err != nil {
					return nil, err
				}
				if jsontext.Value(encoded).IsValid() {
					projected, err := walkNotificationJSON(jsontext.Value(encoded), keep)
					if err != nil {
						return nil, err
					}
					value, err = json.Marshal(string(projected))
					if err != nil {
						return nil, err
					}
				}
			} else {
				var err error
				value, err = walkNotificationJSON(value, keep)
				if err != nil {
					return nil, err
				}
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

// Stream frames recheck notification targets after page projection, immediately
// before release. Membership read during projection is only a snapshot.
func notificationEventStillVisible(ctx context.Context, store db.Storage, event db.Event) (bool, error) {
	scope := issueScopeFromContext(ctx)
	if scope == nil {
		return true, nil
	}
	var uids []string
	_, err := walkNotificationJSON(jsontext.Value(event.Payload), func(slot jsontext.Value) (bool, error) {
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
