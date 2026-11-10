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
	var issueZone string
	if raw, ok := values["timezone"]; ok {
		if err := json.Unmarshal(raw, &issueZone); err != nil {
			return err
		}
	}
	effectiveZone, source := defaultZone, "daemon timezone in config.toml"
	if issueZone != "" {
		effectiveZone, source = issueZone, "issue timezone"
	}
	if effectiveZone == "" {
		effectiveZone = "UTC"
	}
	if portableIssueTimezone(effectiveZone) {
		return nil
	}
	zoneChange, changedZone := diff["timezone"]
	var previousZone string
	if changedZone && zoneChange.From != nil {
		_ = json.Unmarshal(zoneChange.From, &previousZone)
	}
	removedZone := changedZone && zoneChange.To == nil && previousZone != ""
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		if _, changed := diff[field]; !changed && !removedZone {
			continue
		}
		if issueZone == "" && field == "scheduled_on" && recurrenceZone != "" {
			continue
		}
		// Classify just this field with the existing grammar in UTC, so
		// neither the issue zone nor daemon default consults host zone data.
		probe, err := json.Marshal(map[string]jsontext.Value{field: values[field]})
		if err != nil {
			return err
		}
		value, _, _, present, err := metadata.ScheduleFieldInstant(string(probe), field, "UTC")
		if err != nil {
			return fmt.Errorf("%w: %v", metadata.ErrInvalidValue, err)
		}
		// The accepted instant grammar always ends in Z; the resolver above has
		// already classified and validated the value using the shared grammar.
		if present && !strings.HasSuffix(value, "Z") {
			return fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective %s %q)", metadata.ErrInvalidValue, field, source, effectiveZone)
		}
	}
	return nil
}
