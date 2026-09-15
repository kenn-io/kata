package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/notification"
)

type dueNotificationSource struct {
	kind    notification.Kind
	value   string
	present bool
	due     bool
}

// PlanDueNotification returns an ordinary issue metadata patch. An empty map
// means the issue's current inbox state already matches its planning dates.
func PlanDueNotification(
	current json.RawMessage,
	owner string,
	author string,
	now time.Time,
	defaultTimezone string,
	history []Event,
) (map[string]json.RawMessage, error) {
	recipient := owner
	if recipient == "" {
		recipient = author
	}
	recipient, err := notification.NormalizeRecipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("notification recipient: %w", err)
	}

	values := make(map[string]json.RawMessage)
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &values); err != nil {
			return nil, fmt.Errorf("decode issue metadata: %w", err)
		}
	}
	sources := make([]dueNotificationSource, 0, 2)
	for _, kind := range []notification.Kind{notification.Scheduled, notification.Deadline} {
		value, present, due, err := metadata.ScheduleFieldDue(
			string(current), string(kind), now, defaultTimezone,
		)
		if err != nil {
			return nil, fmt.Errorf("evaluate %s: %w", kind, err)
		}
		sources = append(sources, dueNotificationSource{kind: kind, value: value, present: present, due: due})
	}

	patch := make(map[string]json.RawMessage)
	targetKey := notification.MetadataKey(recipient)
	for key, raw := range values {
		automatic, ok := notification.ParseAutomatic(key, raw)
		if !ok {
			continue
		}
		keep := automatic.Recipient == recipient
		if keep {
			keep = false
			for _, source := range sources {
				if source.kind == automatic.Kind && source.present && source.due && source.value == automatic.SourceValue {
					keep = true
					break
				}
			}
		}
		if !keep {
			patch[key] = json.RawMessage("null")
		}
	}

	if raw, occupied := values[targetKey]; occupied && string(raw) != "null" {
		if clearing, ok := patch[targetKey]; !ok || string(clearing) != "null" {
			return patch, nil
		}
	}
	for _, source := range sources {
		if !source.present || !source.due {
			continue
		}
		desired, err := notification.MarshalSystem(source.kind, source.value)
		if err != nil {
			return nil, err
		}
		if dueNotificationDelivered(history, targetKey, desired) {
			continue
		}
		patch[targetKey] = desired
		break
	}
	return patch, nil
}

func dueNotificationDelivered(history []Event, key string, desired json.RawMessage) bool {
	for _, event := range history {
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(event.Payload), &payload) != nil {
			continue
		}
		switch event.Type {
		case "issue.created", "issue.snapshot":
			var values map[string]json.RawMessage
			if json.Unmarshal(payload["metadata"], &values) == nil && equalNotificationValue(values[key], desired) {
				return true
			}
		case "issue.metadata_updated":
			var diff map[string]struct {
				From json.RawMessage `json:"from"`
				To   json.RawMessage `json:"to"`
			}
			if json.Unmarshal(payload["diff"], &diff) != nil {
				continue
			}
			change, ok := diff[key]
			if ok && (equalNotificationValue(change.From, desired) || equalNotificationValue(change.To, desired)) {
				return true
			}
		}
	}
	return false
}

func equalNotificationValue(left, right json.RawMessage) bool {
	if len(left) == 0 || string(left) == "null" {
		return false
	}
	return bytes.Equal(metadata.NormalizeJSON(left), metadata.NormalizeJSON(right))
}
