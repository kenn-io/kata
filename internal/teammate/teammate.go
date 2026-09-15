// Package teammate validates and applies optional teammate attribution.
package teammate

import (
	"errors"
	"maps"
	"regexp"
)

var handlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Validate accepts an absent teammate or one portable teammate handle.
func Validate(value string) error {
	if value == "" || handlePattern.MatchString(value) {
		return nil
	}
	return errors.New("teammate must be 1-64 ASCII letters, digits, dots, underscores or hyphens, starting with a letter or digit")
}

// Resolve selects an explicit override when present, otherwise fallback.
func Resolve(override *string, fallback string) (string, error) {
	value := fallback
	if override != nil {
		value = *override
	}
	return value, Validate(value)
}

// Stamp returns a detached metadata map carrying handle when one is selected.
func Stamp(metadata map[string]any, handle string) (map[string]any, error) {
	if err := Validate(handle); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(metadata)+1)
	maps.Copy(out, metadata)
	if raw, exists := out["teammate"]; exists {
		value, ok := raw.(string)
		if !ok {
			return nil, errors.New("metadata.teammate must be a string")
		}
		if err := Validate(value); err != nil {
			return nil, err
		}
		if handle != "" && value != handle {
			return nil, errors.New("teammate conflicts with initial metadata.teammate")
		}
		return out, nil
	}
	if handle != "" {
		out["teammate"] = handle
	}
	return out, nil
}
