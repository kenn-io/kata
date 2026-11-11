package db

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/internal/recurrence"
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
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
func ValidateRecurrenceTemplate(title string, metadata jsontext.Value) error {
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
	templateMetadata jsontext.Value,
	occurrenceKey string,
	timezone string,
) (jsontext.Value, error) {
	return composeRecurrenceIssueMetadata(templateMetadata, occurrenceKey, timezone, "", true)
}

// ComposeExistingRecurrenceIssueMetadata updates recurrence fields on an
// existing issue while preserving its native recurring delivery metadata.
func ComposeExistingRecurrenceIssueMetadata(
	issueMetadata jsontext.Value,
	occurrenceKey string,
	timezone, defaultZone string,
) (jsontext.Value, error) {
	return composeRecurrenceIssueMetadata(issueMetadata, occurrenceKey, timezone, defaultZone, false)
}

func composeRecurrenceIssueMetadata(
	metadataValue jsontext.Value,
	occurrenceKey string,
	timezone, defaultZone string,
	template bool,
) (jsontext.Value, error) {
	issueTimezone := timezone
	local := timezone == "Local"
	if local {
		var err error
		timezone, err = portableLocalZone()
		if err != nil {
			return nil, err
		}
	}
	object, err := decodeRecurrenceTemplateMetadata(metadataValue)
	if err != nil {
		return nil, err
	}
	scheduledOn, err := metadata.NewScheduleValue(occurrenceKey, timezone)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence scheduled_on: %w", err)
	}
	timezoneValue, err := json.Marshal(issueTimezone)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence timezone: %w", err)
	}
	if local {
		if err := compareLocalSchedule(occurrenceKey, scheduledOn); err != nil {
			return nil, err
		}
	}
	if err := freezeRecurrenceDeadline(object, timezone, defaultZone, local, template); err != nil {
		return nil, err
	}
	object["scheduled_on"] = scheduledOn
	object["timezone"] = timezoneValue

	validate := validateRecurrenceIssueMetadataObject
	if template {
		validate = validateRecurrenceTemplateMetadataObject
	}
	if err := validate(object); err != nil {
		return nil, err
	}
	value, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("marshal recurrence issue metadata: %w", err)
	}
	return value, nil
}

func portableLocalZone() (string, error) {
	zone := time.Local.String()
	if zone == "Local" {
		zone = recurrenceclient.LocalZone(recurrenceclient.DefaultLocaltimePath)
	}
	if zone == "" || zone == "Local" {
		return "", fmt.Errorf("%w: cannot identify Local timezone; change it to a named IANA timezone", ErrInvalidRecurrence)
	}
	if _, err := metadata.ScheduleLocation(zone, metadata.ScheduleTZDataVersion); err != nil {
		return "", fmt.Errorf("%w: cannot save Local timezone %q: %v", ErrInvalidRecurrence, zone, err)
	}
	return zone, nil
}

func freezeRecurrenceDeadline(object map[string]jsontext.Value, recurrenceZone, defaultZone string, local, template bool) error {
	raw := object["deadline_on"]
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	deadline, err := metadata.ScheduleValueText(raw)
	if err != nil {
		return fmt.Errorf("%w: deadline_on: %v", ErrInvalidRecurrence, err)
	}
	if raw[0] != '"' {
		return nil
	}
	zone := recurrenceZone
	if !template {
		zone = defaultZone
		if len(object["timezone"]) > 0 && string(object["timezone"]) != "null" && string(object["timezone"]) != `""` {
			if err := json.Unmarshal(object["timezone"], &zone); err != nil {
				return fmt.Errorf("%w: deadline timezone: %v", ErrInvalidRecurrence, err)
			}
		}
		if zone == "" {
			zone = "UTC"
		}
	}
	localDeadline := template && local || zone == "Local"
	if zone == "Local" {
		zone, err = portableLocalZone()
		if err != nil {
			return err
		}
	}
	object["deadline_on"], err = metadata.NewScheduleValue(deadline, zone)
	if err != nil {
		return fmt.Errorf("%w: deadline_on: %v", ErrInvalidRecurrence, err)
	}
	if localDeadline {
		return compareLocalSchedule(deadline, object["deadline_on"])
	}
	return nil
}

func compareLocalSchedule(value string, saved jsontext.Value) error {
	legacy, err := json.Marshal(map[string]any{"scheduled_on": value, "timezone": "Local"})
	if err != nil {
		return err
	}
	frozen := `{"scheduled_on":` + string(saved) + `}`
	_, _, oldAt, _, err := metadata.ScheduleFieldInstant(string(legacy), "scheduled_on", "")
	if err != nil {
		return fmt.Errorf("%w: resolve Local schedule: %v", ErrInvalidRecurrence, err)
	}
	_, _, newAt, _, err := metadata.ScheduleFieldInstant(frozen, "scheduled_on", "")
	if err != nil {
		return fmt.Errorf("%w: resolve saved schedule: %v", ErrInvalidRecurrence, err)
	}
	oldDate, _, err := metadata.CalendarDate(string(legacy), "scheduled_on", nil, "")
	if err != nil {
		return fmt.Errorf("%w: resolve Local schedule date: %v", ErrInvalidRecurrence, err)
	}
	newDate, _, err := metadata.CalendarDate(frozen, "scheduled_on", nil, "")
	if err != nil {
		return fmt.Errorf("%w: resolve saved schedule date: %v", ErrInvalidRecurrence, err)
	}
	if !oldAt.Equal(newAt) || oldDate != newDate {
		return fmt.Errorf("%w: Local timezone rules differ from the bundled rules for %q; change the recurrence to a named IANA timezone", ErrInvalidRecurrence, value)
	}
	return nil
}

func validatedRecurrenceTemplateMetadata(value jsontext.Value) (map[string]jsontext.Value, error) {
	object, err := decodeRecurrenceTemplateMetadata(value)
	if err != nil {
		return nil, err
	}
	if err := validateRecurrenceTemplateMetadataObject(object); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeRecurrenceTemplateMetadata(value jsontext.Value) (map[string]jsontext.Value, error) {
	if len(value) == 0 {
		return map[string]jsontext.Value{}, nil
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal(value, &object); err != nil {
		return nil, fmt.Errorf("%w: template_metadata must be a JSON object: %v", ErrInvalidRecurrence, err)
	}
	if object == nil {
		return nil, fmt.Errorf("%w: template_metadata must be a JSON object, got null", ErrInvalidRecurrence)
	}
	return object, nil
}

func validateRecurrenceTemplateMetadataObject(object map[string]jsontext.Value) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if recurringMetadataKey(key) {
			return fmt.Errorf("%w: template_metadata %q is system delivery state", ErrInvalidRecurrence, key)
		}
	}
	return validateRecurrenceIssueMetadataObject(object)
}

func validateRecurrenceIssueMetadataObject(object map[string]jsontext.Value) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if recurringMetadataKey(key) {
			continue
		}
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
