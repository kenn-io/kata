package db

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kata/internal/metadata"
)

// portableIssueTimezone admits names from the pinned bundle, never host files.
func portableIssueTimezone(zone string) bool {
	area, _, _ := strings.Cut(zone, "/")
	if zone != "UTC" && zone != "Etc/UTC" && !slices.Contains([]string{"Africa", "America", "Antarctica", "Arctic", "Asia", "Atlantic", "Australia", "Europe", "Indian", "Pacific"}, area) {
		return false
	}
	return bundledScheduleZone(zone)
}

// legacyScheduleZoneKnown reports whether a legacy string schedule resolves to a usable zone.
func legacyScheduleZoneKnown(issueZone jsontext.Value, field, defaultZone, recurrenceZone string) bool {
	var zone string
	if len(issueZone) > 0 && json.Unmarshal(issueZone, &zone) != nil {
		return false
	}
	if field == "scheduled_on" && recurrenceZone != "" && (zone == "" || zone == recurrenceZone) {
		zone = recurrenceZone
	}
	zone = cmp.Or(zone, defaultZone, "UTC")
	return zone == "Local" || bundledScheduleZone(zone)
}

func bundledScheduleZone(zone string) bool {
	_, err := metadata.ScheduleLocation(zone, metadata.ScheduleTZDataVersion)
	return err == nil
}

// PrepareIssueSchedules validates and freezes schedule changes together.
func PrepareIssueSchedules(current jsontext.Value, patch map[string]jsontext.Value, defaultZone, recurrenceZone string) (map[string]jsontext.Value, error) {
	effective, err := ApplyMetadataPatch(current, patch)
	if err != nil {
		return nil, err
	}
	diff, err := metadata.Diff(current, effective)
	if err != nil {
		return nil, err
	}
	var before, after map[string]jsontext.Value
	if len(current) > 0 && string(current) != "null" {
		if err := json.Unmarshal(current, &before); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(effective, &after); err != nil {
		return nil, err
	}
	result := make(map[string]jsontext.Value, len(patch)+2)
	maps.Copy(result, patch)
	zoneChange, changedZone := diff["timezone"]
	if changedZone && zoneChange.To != nil {
		var zone string
		if err := json.Unmarshal(zoneChange.To, &zone); err != nil || !portableIssueTimezone(zone) {
			return nil, fmt.Errorf("%w: issue timezone must be UTC, Etc/UTC, or an exact bundled geographic IANA name", metadata.ErrInvalidValue)
		}
	}
	// Removing an empty zone does not change the effective zone.
	if changedZone && string(zoneChange.From) == `""` && zoneChange.To == nil {
		changedZone = false
	}
	for _, field := range []string{"scheduled_on", "deadline_on"} {
		raw := after[field]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		_, patched := patch[field]
		if !patched && !changedZone {
			continue
		}
		if supplied := patch[field]; len(supplied) > 0 && supplied[0] == '{' {
			saved, err := metadata.DecodeSavedSchedule(supplied)
			if _, prior := metadata.DecodeSavedSchedule(before[field]); err != nil || saved == nil || metadata.IsUnsupportedSchedule(prior) {
				return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, errors.Join(err, prior))
			}
			result[field] = supplied
			continue
		}
		value, err := metadata.ScheduleValueText(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
		}
		if patched && !changedZone && len(before[field]) > 0 {
			previous, err := metadata.ScheduleValueText(before[field])
			if err == nil && previous == value {
				if before[field][0] != '"' || !legacyScheduleZoneKnown(before["timezone"], field, defaultZone, recurrenceZone) {
					result[field] = before[field]
					continue
				}
			}
		}
		if len(before[field]) > 0 {
			saved, err := metadata.DecodeSavedSchedule(before[field])
			if metadata.IsUnsupportedSchedule(err) {
				return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
			}
			if saved != nil && !changedZone {
				result[field], err = metadata.NewScheduleValueWithVersion(value, saved.Timezone, saved.TZDataVersion)
				if err != nil {
					return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
				}
				continue
			}
		}
		var zone string
		_, instantErr := time.Parse(time.RFC3339, value)
		utcInstant := strings.HasSuffix(value, "Z") && instantErr == nil
		source := "daemon timezone in config.toml"
		if len(after["timezone"]) > 0 {
			if err := json.Unmarshal(after["timezone"], &zone); err != nil {
				if !utcInstant {
					return nil, fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective issue timezone %q)", metadata.ErrInvalidValue, field, after["timezone"])
				}
				zone = "UTC"
			}
			if zone != "" {
				source = "issue timezone"
			}
		}
		if field == "scheduled_on" && recurrenceZone != "" && (zone == "" || zone == recurrenceZone) {
			zone = recurrenceZone
			source = "recurrence timezone"
		}
		if zone == "" {
			zone = defaultZone
		}
		if zone == "" {
			zone = "UTC"
		}
		legacyLocal := zone == "Local"
		if legacyLocal {
			zone, err = portableLocalZone()
			if err != nil {
				if !utcInstant {
					return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
				}
				zone = "UTC"
				legacyLocal = false
			}
		}
		if source != "recurrence timezone" && !bundledScheduleZone(zone) {
			if !utcInstant {
				return nil, fmt.Errorf("%w: %s requires a portable issue timezone or daemon timezone in config.toml (effective %s %q)", metadata.ErrInvalidValue, field, source, zone)
			}
			zone = "UTC"
		}
		if source == "recurrence timezone" && utcInstant {
			if _, err := metadata.ScheduleLocation(zone, metadata.ScheduleTZDataVersion); err != nil {
				zone = "UTC"
			}
		}
		result[field], err = metadata.NewScheduleValue(value, zone)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v; set a named IANA %s", metadata.ErrInvalidValue, field, err, source)
		}
		if legacyLocal {
			if err := compareLocalSchedule(value, result[field]); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", metadata.ErrInvalidValue, field, err)
			}
		}
	}
	return result, nil
}
