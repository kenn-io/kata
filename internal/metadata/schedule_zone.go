package metadata

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"time"
)

//go:embed tzdata/zoneinfo.zip
var scheduleZoneBundle []byte

const ScheduleTZDataVersion = "sha256:b2d18a7c8fa8142097a48c99609fb3c92db5ee98bc740294e57eab8ae9f94779"

type savedSchedule struct {
	Value         string `json:"value"`
	Timezone      string `json:"timezone"`
	TZDataVersion string `json:"tzdata_version"`
}

// ScheduleLocation loads the exact zone rules shipped for schedule values.
func ScheduleLocation(zone, version string) (*time.Location, error) {
	if version != ScheduleTZDataVersion {
		return nil, fmt.Errorf("unsupported schedule tzdata version %q", version)
	}

	z, err := zip.NewReader(bytes.NewReader(scheduleZoneBundle), int64(len(scheduleZoneBundle)))
	if err != nil {
		return nil, err
	}
	for _, f := range z.File {
		if f.Name != zone {
			continue
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(r)
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return time.LoadLocationFromTZData(zone, data)
	}
	return nil, fmt.Errorf("unknown schedule timezone %q", zone)
}

// NewScheduleValue freezes the zone rules used to interpret a schedule.
func NewScheduleValue(value, zone string) (jsontext.Value, error) {
	kind, _, _, err := classifyScheduledOn(value)
	if err != nil {
		return nil, err
	}
	if kind == scheduledOnInstant {
		zone = "UTC"
	}
	if zone == "" {
		zone = "UTC"
	}
	if _, err := ScheduleLocation(zone, ScheduleTZDataVersion); err != nil {
		return nil, err
	}
	return json.Marshal(savedSchedule{value, zone, ScheduleTZDataVersion})
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
	var obj savedSchedule
	if err := json.Unmarshal(raw, &obj, json.RejectUnknownMembers(true)); err != nil {
		return nil, nil, err
	}
	kind, _, _, err := classifyScheduledOn(obj.Value)
	if err != nil {
		return nil, nil, err
	}
	if kind == scheduledOnInstant && obj.Timezone != "UTC" {
		return nil, nil, fmt.Errorf("UTC schedule instant requires UTC timezone")
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
