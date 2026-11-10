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
	var values struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(effective, &values); err != nil {
		return err
	}
	if values.Timezone != "" {
		return nil
	}
	if defaultZone == "" {
		defaultZone = "UTC"
	}
	if portableIssueTimezone(defaultZone) {
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
		if field == "scheduled_on" && recurrenceZone != "" {
			continue
		}
		// The existing resolver classifies accepted civil/instant grammar. Resolve
		// with UTC here so admission never consults the nonportable daemon zone.
		value, _, _, present, err := metadata.ScheduleFieldInstant(string(effective), field, "UTC")
		if err != nil {
			return fmt.Errorf("%w: %v", metadata.ErrInvalidValue, err)
		}
		// The accepted instant grammar always ends in Z; the resolver above has
		// already classified and validated the value using the shared grammar.
		if present && !strings.HasSuffix(value, "Z") {
			return fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (current default %q)", metadata.ErrInvalidValue, field, defaultZone)
		}
	}
	return nil
}
