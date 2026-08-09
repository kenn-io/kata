package db

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/kata/internal/metadata"
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

// RecomputeRecurrenceCursor returns the next occurrence for an effective
// schedule. Existing recurrences advance from their last materialized issue;
// recurrences without one start at the schedule's first occurrence.
func RecomputeRecurrenceCursor(rule, dtstart, timezone string, lastOccurrenceKey *string) (*string, error) {
	if lastOccurrenceKey == nil {
		return ValidateRecurrenceCore(rule, dtstart, timezone)
	}
	next, err := recurrence.Walk(rule, dtstart, timezone, *lastOccurrenceKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRecurrence, err)
	}
	return next, nil
}

// ValidateRecurrenceTemplate enforces the invariants required when a template
// is materialized into an issue.
func ValidateRecurrenceTemplate(title string, metadata json.RawMessage) error {
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("%w: template_title must be non-empty", ErrInvalidRecurrence)
	}
	_, err := validatedRecurrenceTemplateMetadata(metadata)
	return err
}

// ComposeRecurrenceIssueMetadata stamps the occurrence's civil date and
// authoritative IANA timezone before validating the resulting issue metadata.
// The order preserves compatibility with templates written by older versions:
// obsolete values in these two generated fields cannot block materialization.
func ComposeRecurrenceIssueMetadata(
	templateMetadata json.RawMessage,
	occurrenceKey string,
	timezone string,
) (json.RawMessage, error) {
	object, err := decodeRecurrenceTemplateMetadata(templateMetadata)
	if err != nil {
		return nil, err
	}
	scheduledOn, err := json.Marshal(occurrenceKey)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence scheduled_on: %w", err)
	}
	timezoneValue, err := json.Marshal(timezone)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence timezone: %w", err)
	}
	object["scheduled_on"] = scheduledOn
	object["timezone"] = timezoneValue
	if err := validateRecurrenceTemplateMetadataObject(object); err != nil {
		return nil, err
	}
	value, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence issue metadata: %w", err)
	}
	return value, nil
}

func validatedRecurrenceTemplateMetadata(value json.RawMessage) (map[string]json.RawMessage, error) {
	object, err := decodeRecurrenceTemplateMetadata(value)
	if err != nil {
		return nil, err
	}
	if err := validateRecurrenceTemplateMetadataObject(object); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeRecurrenceTemplateMetadata(value json.RawMessage) (map[string]json.RawMessage, error) {
	if len(value) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return nil, fmt.Errorf("%w: template_metadata must be a JSON object: %v", ErrInvalidRecurrence, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%w: template_metadata must be a JSON object, got null", ErrInvalidRecurrence)
	}
	return object, nil
}

func validateRecurrenceTemplateMetadataObject(object map[string]json.RawMessage) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, reserved := metadata.IssueRegistry[key]; !reserved {
			continue
		}
		if err := metadata.ValidateCreateValue(metadata.IssueRegistry, key, object[key]); err != nil {
			return fmt.Errorf("%w: template_metadata %q: %v", ErrInvalidRecurrence, key, err)
		}
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
