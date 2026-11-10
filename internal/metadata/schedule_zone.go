package metadata

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"time"
)

//go:embed tzdata/zoneinfo-2026c.zip
var scheduleZoneBundle []byte

const scheduleTZData2026c = "sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"

const ScheduleTZDataVersion = scheduleTZData2026c

type SavedSchedule struct {
	Value         string `json:"value"`
	Timezone      string `json:"timezone"`
	TZDataVersion string `json:"tzdata_version"`
}

// Retain every published bundle when changing the current write version.
var scheduleZoneBundles = map[string][]byte{scheduleTZData2026c: scheduleZoneBundle}

// ScheduleLocation loads the exact zone rules shipped for schedule values.
func ScheduleLocation(zone, version string) (*time.Location, error) {
	return scheduleLocationFromBundles(scheduleZoneBundles, zone, version)
}

func scheduleLocationFromBundles(bundles map[string][]byte, zone, version string) (*time.Location, error) {
	bundle, ok := bundles[version]
	if !ok {
		return nil, fmt.Errorf("unsupported schedule tzdata version %q", version)
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
	if len(raw) == 0 || raw[0] != '{' {
		return nil, nil
	}
	var obj SavedSchedule
	if err := json.Unmarshal(raw, &obj, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if _, _, _, err := classifyScheduledOn(obj.Value); err != nil {
		return nil, err
	}
	if _, err := ScheduleLocation(obj.Timezone, obj.TZDataVersion); err != nil {
		return nil, err
	}
	return &obj, nil
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
	obj, err := DecodeSavedSchedule(raw)
	if err != nil {
		return nil, nil, err
	}
	if obj == nil {
		return nil, nil, fmt.Errorf("schedule must be a string or object")
	}
	loc, err := ScheduleLocation(obj.Timezone, obj.TZDataVersion)
	if err != nil {
		return nil, nil, err
	}
	return &obj.Value, loc, nil
}

// ScheduleValueText returns the editable text of a stored schedule.
func ScheduleValueText(raw jsontext.Value) (string, error) {
	value, _, err := decodeScheduleValue(raw)
	if err != nil || value == nil {
		return "", err
	}
	return *value, nil
}
