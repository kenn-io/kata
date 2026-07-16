package db

import (
	"encoding/json"
	"fmt"
)

// ApplyMetadataPatch merges patch keys into an existing metadata object.
// JSON null removes a key; an empty or null current value is treated as {}.
func ApplyMetadataPatch(current json.RawMessage, patch map[string]json.RawMessage) (json.RawMessage, error) {
	var values map[string]json.RawMessage
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &values); err != nil {
			return nil, fmt.Errorf("unmarshal current metadata: %w", err)
		}
	}
	if values == nil {
		values = make(map[string]json.RawMessage)
	}
	for key, value := range patch {
		if string(value) == "null" {
			delete(values, key)
			continue
		}
		values[key] = value
	}
	result, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal new metadata: %w", err)
	}
	return result, nil
}
