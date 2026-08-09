package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	_ "time/tzdata" // embed IANA tz database so LoadLocation works without system tzdata
)

// ErrInvalidValue is returned when a value fails type-specific validation
// against a server-reserved key.
var ErrInvalidValue = errors.New("invalid value")

// reULID matches a 26-character Crockford base32 ULID.
var reULID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// ValidateCreateValue validates raw as a metadata value supplied at creation
// time. Unlike Validate — which accepts a JSON null as "clear this key" — an
// empty or null value is rejected here: at creation there is nothing to clear,
// so a null value is a caller error. The rejection error wraps ErrInvalidValue
// (as every validation error does) so callers can map it to a 400. Non-null
// values delegate to Validate. Callers add their own framing/context; the
// returned error carries only the "null values are not allowed at creation"
// phrasing, no per-key prefix.
func ValidateCreateValue(registry map[string]Entry, key string, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%w: null values are not allowed at creation", ErrInvalidValue)
	}
	return Validate(registry, key, raw)
}

// Validate checks raw against the validator for key, if any. Reserved keys
// (those present in registry) go through their type-specific validator; any
// other key is accepted as an opaque pass-through value and Validate returns
// nil. A JSON null value is always accepted and signals "clear this key".
func Validate(registry map[string]Entry, key string, raw json.RawMessage) error {
	entry, ok := registry[key]
	if !ok {
		// Unknown / unreserved keys are accepted opaquely. The daemon stores
		// them verbatim so consumers can carry their own metadata without a
		// schema release.
		return nil
	}
	if string(raw) == "null" {
		return nil
	}
	switch entry.Type {
	case TypeDate:
		return validateDate(raw)
	case TypeBool:
		return validateBool(raw)
	case TypeString:
		return validateString(raw)
	case TypeChecklist:
		return validateChecklist(raw)
	case TypeTimezoneIANA:
		return validateTimezoneIANA(raw)
	case TypeSchedule:
		return validateSchedule(raw)
	default:
		return fmt.Errorf("no validator for type %d", entry.Type)
	}
}

func validateSchedule(raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%w: schedule must be a JSON string: %v", ErrInvalidValue, err)
	}
	if _, _, _, err := classifyScheduledOn(s); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return nil
}

func validateDate(raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%w: date must be a JSON string: %v", ErrInvalidValue, err)
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return fmt.Errorf("%w: date must match YYYY-MM-DD: %v", ErrInvalidValue, err)
	}
	return nil
}

func validateBool(raw json.RawMessage) error {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("%w: value must be a JSON boolean: %v", ErrInvalidValue, err)
	}
	return nil
}

func validateString(raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%w: value must be a JSON string: %v", ErrInvalidValue, err)
	}
	return nil
}

// checklistItem is one entry in a TypeChecklist array.
type checklistItem struct {
	ID   string  `json:"id"`
	Text *string `json:"text"`
	Done *bool   `json:"done"`
}

func validateChecklist(raw json.RawMessage) error {
	var items []checklistItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("%w: checklist must be an array of items: %v", ErrInvalidValue, err)
	}
	for i, it := range items {
		if !reULID.MatchString(it.ID) {
			return fmt.Errorf("%w: item[%d].id must be a 26-char ULID, got %q",
				ErrInvalidValue, i, it.ID)
		}
		if it.Text == nil {
			return fmt.Errorf("%w: item[%d].text required", ErrInvalidValue, i)
		}
		if it.Done == nil {
			return fmt.Errorf("%w: item[%d].done required", ErrInvalidValue, i)
		}
	}
	return nil
}

func validateTimezoneIANA(raw json.RawMessage) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("%w: timezone must be a JSON string: %v", ErrInvalidValue, err)
	}
	if _, err := time.LoadLocation(s); err != nil {
		return fmt.Errorf("%w: timezone %q is not a valid IANA name: %v", ErrInvalidValue, s, err)
	}
	return nil
}
