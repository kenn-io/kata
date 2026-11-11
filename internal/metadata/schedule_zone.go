package metadata

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"time"
)

//go:embed tzdata/zoneinfo-2026c.zip
var scheduleZoneBundle []byte

const scheduleTZData2026c = "sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"

// ScheduleTZDataVersion identifies the rules used for newly saved schedules.
const ScheduleTZDataVersion = scheduleTZData2026c

// ErrUnsupportedScheduleTZData marks a saved schedule whose tzdata version this
// build does not ship.
var ErrUnsupportedScheduleTZData = errors.New("unsupported schedule tzdata version")

// SavedSchedule records a schedule and the rules used to interpret it.
type SavedSchedule struct {
	Value         string `json:"value"`
	Timezone      string `json:"timezone"`
	TZDataVersion string `json:"tzdata_version"`
}

// Retain every published bundle when changing the current write version.
var scheduleZoneBundles = map[string][]byte{scheduleTZData2026c: scheduleZoneBundle}
var scheduleLocations sync.Map

// ScheduleLocation loads the exact zone rules shipped for schedule values.
func ScheduleLocation(zone, version string) (*time.Location, error) {
	key := [2]string{zone, version}
	if loc, ok := scheduleLocations.Load(key); ok {
		return loc.(*time.Location), nil
	}
	loc, err := scheduleLocationFromBundles(scheduleZoneBundles, zone, version)
	if err != nil {
		return nil, err
	}
	actual, _ := scheduleLocations.LoadOrStore(key, loc)
	return actual.(*time.Location), nil
}

func scheduleLocationFromBundles(bundles map[string][]byte, zone, version string) (*time.Location, error) {
	bundle, ok := bundles[version]
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnsupportedScheduleTZData, version)
	}
	z, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		return nil, err
	}
	data, err := fs.ReadFile(z, zone)
	if err != nil {
		return nil, fmt.Errorf("unknown schedule timezone %q: %w", zone, err)
	}
	return time.LoadLocationFromTZData(zone, data)
}

// NewScheduleValue freezes the zone rules used to interpret a schedule.
func NewScheduleValue(value, zone string) (jsontext.Value, error) {
	return NewScheduleValueWithVersion(value, zone, ScheduleTZDataVersion)
}

// NewScheduleValueWithVersion preserves the rules of an edited schedule.
func NewScheduleValueWithVersion(value, zone, version string) (jsontext.Value, error) {
	if _, _, _, err := classifyScheduledOn(value); err != nil {
		return nil, err
	}
	if zone == "" {
		zone = "UTC"
	}
	if _, err := ScheduleLocation(zone, version); err != nil {
		return nil, err
	}
	return json.Marshal(SavedSchedule{value, zone, version})
}

// DecodeSavedSchedule returns a descriptor for an object-backed value.
func DecodeSavedSchedule(raw jsontext.Value) (*SavedSchedule, error) {
	obj, _, err := decodeSavedSchedule(raw)
	return obj, err
}

// SavedScheduleWallTime projects saved instants using their saved rules.
func SavedScheduleWallTime(raw jsontext.Value) (string, error) {
	obj, loc, err := decodeSavedSchedule(raw)
	if err != nil || obj == nil {
		return "", err
	}
	kind, _, instant, err := classifyScheduledOn(obj.Value)
	if err != nil || kind != scheduledOnInstant {
		return "", err
	}
	return instant.In(loc).Format("2006-01-02T15:04:05.999999999"), nil
}

// IsUnsupportedSchedule reports a saved schedule written by a newer build:
// tzdata rules or object members this build does not know.
func IsUnsupportedSchedule(err error) bool {
	return errors.Is(err, ErrUnsupportedScheduleTZData) || errors.Is(err, json.ErrUnknownName)
}

func decodeSavedSchedule(raw jsontext.Value) (*SavedSchedule, *time.Location, error) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil, nil
	}
	var obj SavedSchedule
	if err := json.Unmarshal(raw, &obj, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, err
	}
	if _, _, _, err := classifyScheduledOn(obj.Value); err != nil {
		return nil, nil, err
	}
	loc, err := ScheduleLocation(obj.Timezone, obj.TZDataVersion)
	if err != nil {
		return nil, nil, err
	}
	return &obj, loc, nil
}

func decodeScheduleValue(raw jsontext.Value) (*string, *time.Location, error) {
	if string(raw) == "null" || len(raw) == 0 {
		return nil, nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, err
		}
		return &s, nil, nil
	}
	obj, loc, err := decodeSavedSchedule(raw)
	if err != nil {
		return nil, nil, err
	}
	if obj == nil {
		return nil, nil, fmt.Errorf("schedule must be a string or object")
	}
	return &obj.Value, loc, nil
}

// SavedScheduleZone returns the zone a saved schedule was written in, "" for
// a plain or absent value, and an error for rules this build cannot read.
func SavedScheduleZone(raw jsontext.Value) (string, error) {
	obj, err := DecodeSavedSchedule(raw)
	if obj == nil {
		return "", err
	}
	return obj.Timezone, nil
}

// ScheduleClockText is the stored text of a schedule, with a saved UTC instant
// read as the clock time of its saved zone; the error's text when its rules
// are unreadable, so the detail strip says why, and "" when absent.
func ScheduleClockText(raw jsontext.Value) string {
	value, loc, err := decodeScheduleValue(raw)
	if value == nil {
		return errorText(err)
	}
	if kind, _, instant, _ := classifyScheduledOn(*value); kind == scheduledOnInstant && loc != nil {
		return instant.In(loc).Format("2006-01-02T15:04:05.999999999")
	}
	return *value
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ScheduleValueText returns the editable text of a stored schedule.
func ScheduleValueText(raw jsontext.Value) (string, error) {
	value, _, err := decodeScheduleValue(raw)
	if err != nil || value == nil {
		return "", err
	}
	return *value, nil
}
