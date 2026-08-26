package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/metadata"
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

// CheckMetadataPatchGuard compares one guard with the current metadata blob.
// Callers must invoke it after reading the row inside the same transaction as
// the subsequent write. A nil guard leaves the patch unconditional.
func CheckMetadataPatchGuard(current json.RawMessage, patch map[string]json.RawMessage, guard *MetadataPatchGuard) error {
	if guard == nil {
		return nil
	}
	if strings.TrimSpace(guard.Key) == "" {
		return errors.New("metadata guard key must not be empty")
	}
	if _, ok := patch[guard.Key]; !ok {
		return fmt.Errorf("metadata guard key %q is not patched", guard.Key)
	}
	hasValue := len(guard.IfValue) > 0
	if hasValue == guard.IfAbsent {
		return errors.New("metadata guard must set exactly one of if_value or if_absent")
	}
	if hasValue && strings.TrimSpace(string(guard.IfValue)) == "null" {
		return errors.New("metadata guard if_value must not be null")
	}

	var values map[string]json.RawMessage
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &values); err != nil {
			return fmt.Errorf("unmarshal current metadata for guard: %w", err)
		}
	}
	value, present := values[guard.Key]
	if present && string(value) == "null" {
		present = false
	}
	if guard.IfAbsent {
		if present {
			return &MetadataGuardConflictError{Key: guard.Key}
		}
		return nil
	}
	if !present || !bytes.Equal(metadata.NormalizeJSON(value), metadata.NormalizeJSON(guard.IfValue)) {
		return &MetadataGuardConflictError{Key: guard.Key}
	}
	return nil
}
