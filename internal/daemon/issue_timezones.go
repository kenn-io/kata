package daemon

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/metadata"
)

// portableIssueTimezone admits names from the pinned bundle, never host files.
func portableIssueTimezone(zone string) bool {
	area, _, _ := strings.Cut(zone, "/")
	if zone != "UTC" && zone != "Etc/UTC" && !slices.Contains([]string{"Africa", "America", "Antarctica", "Arctic", "Asia", "Atlantic", "Australia", "Europe", "Indian", "Pacific"}, area) {
		return false
	}
	_, err := metadata.ScheduleLocation(zone, metadata.ScheduleTZDataVersion)
	return err == nil
}

// issueTimezoneWriteRelevant excludes unrelated agent metadata from admission.
func issueTimezoneWriteRelevant(patch map[string]jsontext.Value) bool {
	for _, key := range []string{"timezone", "scheduled_on", "deadline_on"} {
		if _, ok := patch[key]; ok {
			return true
		}
	}
	return false
}

// validateIssueTimezoneWrite is admission policy, not stored-data validation.
// In particular, an unchanged legacy timezone remains usable and echoable.
func validateIssueTimezoneWrite(current jsontext.Value, patch map[string]jsontext.Value, defaultZone, recurrenceZone string) error {
	effective, err := db.ApplyMetadataPatch(current, patch)
	if err != nil {
		return err
	}
	diff, err := metadata.Diff(current, effective)
	if err != nil {
		return err
	}
	if change, ok := diff["timezone"]; ok && change.To != nil {
		var zone string
		if err := json.Unmarshal(change.To, &zone); err != nil || !portableIssueTimezone(zone) {
			return fmt.Errorf("%w: issue timezone must be UTC, Etc/UTC, or an exact bundled geographic IANA name", metadata.ErrInvalidValue)
		}
	}
	var values map[string]jsontext.Value
	if err := json.Unmarshal(effective, &values); err != nil {
		return err
	}
	// Untouched restored values need interpretation only for civil candidates.
	zoneValue := func(raw jsontext.Value) string {
		var zone string
		if err := json.Unmarshal(raw, &zone); err != nil {
			return string(raw) // Present but unreadable is nonportable, never absent.
		}
		return zone
	}
	zoneChange, changedZone := diff["timezone"]
	removedZone := changedZone && zoneChange.To == nil && zoneValue(zoneChange.From) != ""
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		if _, changed := diff[field]; !changed && !removedZone {
			continue
		}
		raw := values[field]
		if len(raw) == 0 {
			continue
		}
		// The patch hook precedes shared grammar validation; invalid changed values
		// still fail before commit. Classify exposed stored values conservatively.
		value, valueErr := metadata.ScheduleValueText(raw)
		if valueErr == nil && strings.HasSuffix(value, "Z") {
			continue
		}
		issueZone := zoneValue(values["timezone"])
		if field == "scheduled_on" && recurrenceZone != "" && (issueZone == "" || issueZone == recurrenceZone) {
			continue
		}
		effectiveZone, source := defaultZone, "daemon timezone in config.toml"
		if issueZone != "" {
			effectiveZone, source = issueZone, "issue timezone"
		}
		if effectiveZone == "" {
			effectiveZone = "UTC"
		}
		if !portableIssueTimezone(effectiveZone) {
			return fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective %s %q)", metadata.ErrInvalidValue, field, source, effectiveZone)
		}
	}
	return nil
}

// freezeIssueSchedules saves the effective rules with changed civil values.
func freezeIssueSchedules(current jsontext.Value, patch map[string]jsontext.Value, defaultZone, recurrenceZone string) (map[string]jsontext.Value, error) {
	effective, err := db.ApplyMetadataPatch(current, patch)
	if err != nil {
		return nil, err
	}
	var before, after map[string]jsontext.Value
	if len(current) > 0 {
		if err := json.Unmarshal(current, &before); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(effective, &after); err != nil {
		return nil, err
	}
	result := make(map[string]jsontext.Value, len(patch)+2)
	for k, v := range patch {
		result[k] = v
	}
	_, zonePatched := patch["timezone"]
	changedZone := zonePatched && string(before["timezone"]) != string(after["timezone"]) && !(string(before["timezone"]) == `""` && len(after["timezone"]) == 0)
	var issueZone string
	zoneErr := json.Unmarshal(after["timezone"], &issueZone)
	if len(after["timezone"]) == 0 {
		zoneErr = nil
	}
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		raw, exists := after[field]
		if !exists || string(raw) == "null" {
			continue
		}
		_, patched := patch[field]
		if !patched && !changedZone {
			continue
		}
		value, err := metadata.ScheduleValueText(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
		}
		if patched && !changedZone && len(before[field]) > 0 {
			previous, err := metadata.ScheduleValueText(before[field])
			if err == nil && previous == value {
				result[field] = before[field]
				continue
			}
		}
		if strings.HasSuffix(value, "Z") {
			stamped, err := metadata.NewScheduleValue(value, "UTC")
			if err != nil {
				return nil, fmt.Errorf("%w: %v", metadata.ErrInvalidValue, err)
			}
			result[field] = stamped
			continue
		}
		if zoneErr != nil {
			return nil, fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective issue timezone %q)", metadata.ErrInvalidValue, field, after["timezone"])
		}
		zone := issueZone
		if zone == "" && field == "scheduled_on" && recurrenceZone != "" {
			zone = recurrenceZone
		}
		if zone == "" {
			zone = defaultZone
		}
		if recurrenceZone == "" || zone != recurrenceZone {
			if !portableIssueTimezone(zone) && zone != "" {
				return nil, fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective issue timezone %q)", metadata.ErrInvalidValue, field, zone)
			}
		}
		stamped, err := metadata.NewScheduleValue(value, zone)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", metadata.ErrInvalidValue, err)
		}
		result[field] = stamped
	}
	return result, nil
}
