package notification

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
)

// References returns comment UIDs named by a notification slot or diff.
func References(raw jsontext.Value) []string {
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
			out = append(out, References(object[key])...)
		}
	}
	return out
}

// WalkSlots visits notification slots in issue metadata or a metadata diff.
// Values below other keys remain opaque.
func WalkSlots(slots map[string]jsontext.Value, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	for key, value := range slots {
		if !strings.HasPrefix(key, KeyPrefix) {
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
	return json.Marshal(slots, json.Deterministic(true))
}

// WalkEventPayload visits notification slots in a supported event payload.
func WalkEventPayload(raw jsontext.Value, eventType string, keep func(jsontext.Value) (bool, error)) (jsontext.Value, error) {
	if eventType != "issue.metadata_updated" && eventType != "issue.created" && eventType != "issue.snapshot" && eventType != "issue.restated" {
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
		projected, err := WalkEventPayload(jsontext.Value(encoded), eventType, keep)
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
	projected, err := WalkSlots(slots, keep)
	if err != nil {
		return nil, err
	}
	payload[field] = projected
	return json.Marshal(payload, json.Deterministic(true))
}
