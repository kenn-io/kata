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
	_, ok := slices.BinarySearch(portableIssueTimezones[:], zone)
	return ok
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
	if !issueTimezoneWriteRelevant(patch) {
		return nil
	}
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
		var value *string
		if err := json.Unmarshal(raw, &value); err == nil && (value == nil || strings.HasSuffix(*value, "Z")) {
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
