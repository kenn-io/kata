// Package rootbridge translates planning fields between Kata metadata and a
// connector's typed field values.
package rootbridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
	"go.kenn.io/kata/pkg/connector"
)

const (
	fieldKindNull          = "null"
	fieldKindDate          = "date"
	fieldKindLocalDateTime = "local_datetime"
	fieldKindInstant       = "instant"

	localMinuteLayout = "2006-01-02T15:04"
	localSecondLayout = "2006-01-02T15:04:05"
)

// FieldCodec translates one Kata planning field to and from connector values.
type FieldCodec interface {
	KataField() string
	ReadKata(db.Issue) (connector.FieldValue, error)
	KataPatch(db.Issue, connector.FieldValue) (map[string]json.RawMessage, error)
	ValidateExternalDescriptor(connector.FieldDescriptor) error
}

var fieldCodecs = map[string]FieldCodec{
	"scheduled_on": scheduleCodec{key: "scheduled_on"},
	"deadline_on":  scheduleCodec{key: "deadline_on"},
}

type scheduleCodec struct {
	key string
}

func (c scheduleCodec) KataField() string {
	return c.key
}

func (c scheduleCodec) ReadKata(issue db.Issue) (connector.FieldValue, error) {
	values, err := metadataObject(issue.Metadata)
	if err != nil {
		return connector.FieldValue{}, err
	}
	raw, ok := values[c.key]
	if !ok || string(raw) == "null" {
		return nullFieldValue(), nil
	}
	if err := metadata.Validate(metadata.IssueRegistry, c.key, raw); err != nil {
		return connector.FieldValue{}, fmt.Errorf("validate %s: %w", c.key, err)
	}

	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return connector.FieldValue{}, fmt.Errorf("decode %s: %w", c.key, err)
	}
	field, err := kataScheduleFieldValue(value, values)
	if err != nil {
		return connector.FieldValue{}, fmt.Errorf("decode %s: %w", c.key, err)
	}
	return field, nil
}

func (c scheduleCodec) KataPatch(issue db.Issue, value connector.FieldValue) (map[string]json.RawMessage, error) {
	return c.kataPatch(issue, value, "")
}

func (c scheduleCodec) kataPatch(
	issue db.Issue,
	value connector.FieldValue,
	coordinatedTimezone string,
) (map[string]json.RawMessage, error) {
	canonical, err := canonicalFieldValue(value)
	if err != nil {
		return nil, err
	}
	patch := map[string]json.RawMessage{c.key: mustJSON(canonicalScheduleValue(canonical))}
	if canonical.Kind != fieldKindLocalDateTime {
		return patch, nil
	}

	values, err := metadataObject(issue.Metadata)
	if err != nil {
		return nil, err
	}
	otherKey := "scheduled_on"
	if c.key == otherKey {
		otherKey = "deadline_on"
	}
	if other, err := kataScheduleFieldValueFromMetadata(otherKey, values); err != nil {
		return nil, err
	} else if other.Kind == fieldKindLocalDateTime {
		if other.Timezone != canonical.Timezone && coordinatedTimezone != canonical.Timezone {
			return nil, fmt.Errorf("%s local timezone %q conflicts with %s timezone %q", c.key, canonical.Timezone, otherKey, other.Timezone)
		}
	} else if other.Kind == fieldKindDate {
		timezone, present, err := optionalMetadataTimezone(values)
		if err != nil {
			return nil, fmt.Errorf("read %s timezone: %w", otherKey, err)
		}
		if present && timezone != canonical.Timezone && coordinatedTimezone != canonical.Timezone {
			return nil, fmt.Errorf("%s local timezone %q conflicts with %s timezone %q", c.key, canonical.Timezone, otherKey, timezone)
		}
	}
	patch["timezone"] = mustJSON(canonical.Timezone)
	return patch, nil
}

func kataPatchWithCoordinatedTimezone(
	codec FieldCodec,
	issue db.Issue,
	value connector.FieldValue,
	coordinatedTimezone string,
) (map[string]json.RawMessage, error) {
	if schedule, ok := codec.(scheduleCodec); ok {
		return schedule.kataPatch(issue, value, coordinatedTimezone)
	}
	return codec.KataPatch(issue, value)
}

func (c scheduleCodec) ValidateExternalDescriptor(descriptor connector.FieldDescriptor) error {
	if strings.TrimSpace(descriptor.ID) == "" || strings.TrimSpace(descriptor.DisplayName) == "" || strings.TrimSpace(descriptor.SchemaRevision) == "" {
		return fmt.Errorf("field descriptor identity is required")
	}
	if !descriptor.Nullable || !descriptor.Writable {
		return fmt.Errorf("field descriptor must be nullable and writable")
	}
	if len(descriptor.AcceptedKinds) == 0 {
		return fmt.Errorf("field descriptor accepted kinds are required")
	}
	seen := make(map[string]struct{}, len(descriptor.AcceptedKinds))
	for _, kind := range descriptor.AcceptedKinds {
		canonicalKind := strings.TrimSpace(kind)
		if kind != canonicalKind {
			return fmt.Errorf("external field kind %q is not canonical", kind)
		}
		kind = canonicalKind
		switch kind {
		case fieldKindDate, fieldKindLocalDateTime, fieldKindInstant:
		default:
			return fmt.Errorf("unsupported external field kind %q", kind)
		}
		if _, duplicate := seen[kind]; duplicate {
			return fmt.Errorf("duplicate external field kind %q", kind)
		}
		seen[kind] = struct{}{}
	}
	return nil
}

func metadataObject(raw db.JSONBlob) (map[string]json.RawMessage, error) {
	if raw == "" {
		return map[string]json.RawMessage{}, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("decode issue metadata: %w", err)
	}
	if values == nil {
		return map[string]json.RawMessage{}, nil
	}
	return values, nil
}

func kataScheduleFieldValue(value string, metadataValues map[string]json.RawMessage) (connector.FieldValue, error) {
	field := connector.FieldValue{Value: value}
	switch scheduleKind(value) {
	case fieldKindDate:
		field.Kind = fieldKindDate
	case fieldKindInstant:
		field.Kind = fieldKindInstant
	case fieldKindLocalDateTime:
		timezone, err := metadataTimezone(metadataValues)
		if err != nil {
			return connector.FieldValue{}, err
		}
		field.Kind = fieldKindLocalDateTime
		field.Timezone = timezone
	default:
		return connector.FieldValue{}, fmt.Errorf("invalid schedule value %q", value)
	}
	return canonicalFieldValue(field)
}

func kataScheduleFieldValueFromMetadata(key string, values map[string]json.RawMessage) (connector.FieldValue, error) {
	raw, ok := values[key]
	if !ok || string(raw) == "null" {
		return nullFieldValue(), nil
	}
	if err := metadata.Validate(metadata.IssueRegistry, key, raw); err != nil {
		return connector.FieldValue{}, fmt.Errorf("validate %s: %w", key, err)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return connector.FieldValue{}, fmt.Errorf("decode %s: %w", key, err)
	}
	return kataScheduleFieldValue(value, values)
}

func metadataTimezone(values map[string]json.RawMessage) (string, error) {
	timezone, present, err := optionalMetadataTimezone(values)
	if err != nil {
		return "", err
	}
	if !present {
		return "", fmt.Errorf("local date-time requires issue timezone")
	}
	return timezone, nil
}

func optionalMetadataTimezone(values map[string]json.RawMessage) (string, bool, error) {
	raw, ok := values["timezone"]
	if !ok || string(raw) == "null" {
		return "", false, nil
	}
	if err := metadata.Validate(metadata.IssueRegistry, "timezone", raw); err != nil {
		return "", false, err
	}
	var timezone string
	if err := json.Unmarshal(raw, &timezone); err != nil {
		return "", false, fmt.Errorf("decode timezone: %w", err)
	}
	return timezone, true, nil
}

func canonicalFieldValue(value connector.FieldValue) (connector.FieldValue, error) {
	if value.Kind == "" || value.Kind == fieldKindNull {
		if value.Value != "" || value.Timezone != "" {
			return connector.FieldValue{}, fmt.Errorf("null field value must not contain a value or timezone")
		}
		return nullFieldValue(), nil
	}
	switch value.Kind {
	case fieldKindDate:
		if value.Timezone != "" {
			return connector.FieldValue{}, fmt.Errorf("date field value must not contain a timezone")
		}
		parsed, err := time.Parse(time.DateOnly, value.Value)
		if err != nil || parsed.Format(time.DateOnly) != value.Value {
			return connector.FieldValue{}, fmt.Errorf("date field value %q must use YYYY-MM-DD", value.Value)
		}
		return connector.FieldValue{Kind: fieldKindDate, Value: parsed.Format(time.DateOnly)}, nil
	case fieldKindLocalDateTime:
		layout := localScheduleLayout(value.Value)
		if layout == "" {
			return connector.FieldValue{}, fmt.Errorf("local date-time %q must use minute or second precision", value.Value)
		}
		if value.Timezone == "" {
			return connector.FieldValue{}, fmt.Errorf("local date-time requires timezone")
		}
		if _, err := time.LoadLocation(value.Timezone); err != nil {
			return connector.FieldValue{}, fmt.Errorf("load timezone %q: %w", value.Timezone, err)
		}
		return connector.FieldValue{Kind: fieldKindLocalDateTime, Value: value.Value, Timezone: value.Timezone}, nil
	case fieldKindInstant:
		if value.Timezone != "" {
			return connector.FieldValue{}, fmt.Errorf("instant field value must not contain a timezone")
		}
		if !strings.HasSuffix(value.Value, "Z") {
			return connector.FieldValue{}, fmt.Errorf("instant %q must use UTC Z", value.Value)
		}
		parsed, err := time.Parse(time.RFC3339Nano, value.Value)
		if err != nil {
			return connector.FieldValue{}, fmt.Errorf("parse instant %q: %w", value.Value, err)
		}
		return connector.FieldValue{Kind: fieldKindInstant, Value: parsed.UTC().Format(time.RFC3339Nano)}, nil
	default:
		return connector.FieldValue{}, fmt.Errorf("unsupported field value kind %q", value.Kind)
	}
}

func scheduleKind(value string) string {
	if parsed, err := time.Parse(time.DateOnly, value); err == nil && parsed.Format(time.DateOnly) == value {
		return fieldKindDate
	}
	if localScheduleLayout(value) != "" {
		return fieldKindLocalDateTime
	}
	if strings.HasSuffix(value, "Z") {
		if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return fieldKindInstant
		}
	}
	return ""
}

func localScheduleLayout(value string) string {
	for _, layout := range []string{localMinuteLayout, localSecondLayout} {
		if parsed, err := time.Parse(layout, value); err == nil && parsed.Format(layout) == value {
			return layout
		}
	}
	return ""
}

func canonicalScheduleValue(value connector.FieldValue) any {
	if value.Kind == fieldKindNull {
		return nil
	}
	return value.Value
}

func nullFieldValue() connector.FieldValue {
	return connector.FieldValue{Kind: fieldKindNull}
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
