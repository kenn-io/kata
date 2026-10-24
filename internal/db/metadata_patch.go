package db

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kata/internal/metadata"
)

// ApplyMetadataPatch merges patch keys into an existing metadata object.
// JSON null removes a key; an empty or null current value is treated as {}.
func ApplyMetadataPatch(current jsontext.Value, patch map[string]jsontext.Value) (jsontext.Value, error) {
	var values map[string]jsontext.Value
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &values); err != nil {
			return nil, fmt.Errorf("unmarshal current metadata: %w", err)
		}
	}
	if values == nil {
		values = make(map[string]jsontext.Value)
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

// Validate checks the guard's shape without comparing current metadata or a
// proposed patch. Native writes still check both inside their transaction.
func (guard MetadataPatchGuard) Validate() error {
	if err := guard.validateKey(); err != nil {
		return err
	}
	return guard.validateCondition()
}

func (guard MetadataPatchGuard) validateKey() error {
	if strings.TrimSpace(guard.Key) == "" {
		return errors.New("metadata guard key must not be empty")
	}
	return nil
}

func (guard MetadataPatchGuard) validateCondition() error {
	hasValue := len(guard.IfValue) > 0
	if hasValue == guard.IfAbsent {
		return errors.New("metadata guard must set exactly one of if_value or if_absent")
	}
	if hasValue && strings.TrimSpace(string(guard.IfValue)) == "null" {
		return errors.New("metadata guard if_value must not be null")
	}
	return nil
}

// CheckMetadataPatchGuard compares one guard with the current metadata blob.
// Callers must invoke it after reading the row inside the same transaction as
// the subsequent write. A nil guard leaves the patch unconditional.
func CheckMetadataPatchGuard(current jsontext.Value, patch map[string]jsontext.Value, guard *MetadataPatchGuard) error {
	if guard == nil {
		return nil
	}
	if err := guard.validateKey(); err != nil {
		return err
	}
	if _, ok := patch[guard.Key]; !ok {
		return fmt.Errorf("metadata guard key %q is not patched", guard.Key)
	}
	if err := guard.validateCondition(); err != nil {
		return err
	}

	var values map[string]jsontext.Value
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
