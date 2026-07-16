package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/recurrence"
)

// ValidateRecurrenceCore validates a rule, start date, and timezone together
// and returns the first occurrence used to initialize the recurrence cursor.
func ValidateRecurrenceCore(rule, dtstart, timezone string) (*string, error) {
	first, err := recurrence.Next(rule, dtstart, timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecurrence, err)
	}
	return first, nil
}

// ValidateRecurrenceTemplate enforces the invariants required when a template
// is materialized into an issue.
func ValidateRecurrenceTemplate(title string, metadata json.RawMessage) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("%w: template_title must be non-empty", ErrInvalidRecurrence)
	}
	if len(metadata) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &object); err != nil {
		return fmt.Errorf("%w: template_metadata must be a JSON object: %v", ErrInvalidRecurrence, err)
	}
	if object == nil {
		return fmt.Errorf("%w: template_metadata must be a JSON object, got null", ErrInvalidRecurrence)
	}
	return nil
}

// NormalizeRecurrenceLabels validates, normalizes, sorts, and de-duplicates
// recurrence template labels using the issue-label alphabet.
func NormalizeRecurrenceLabels(labels []string) ([]string, error) {
	seen := make(map[string]struct{}, len(labels))
	normalized := make([]string, 0, len(labels))
	for _, raw := range labels {
		label := strings.TrimSpace(strings.ToLower(raw))
		if len(label) == 0 || len(label) > 64 {
			return nil, fmt.Errorf("%w: label %q must be 1-64 characters", ErrLabelInvalid, label)
		}
		for _, char := range label {
			if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || strings.ContainsRune("._:-", char) {
				continue
			}
			return nil, fmt.Errorf("%w: label %q contains invalid characters", ErrLabelInvalid, label)
		}
		if _, exists := seen[label]; exists {
			continue
		}
		seen[label] = struct{}{}
		normalized = append(normalized, label)
	}
	sort.Strings(normalized)
	return normalized, nil
}
