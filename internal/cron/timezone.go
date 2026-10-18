package cron

import (
	"fmt"
	"strings"

	// Keep bundled timezone data available when the host has no zoneinfo files.
	_ "time/tzdata"
)

//go:generate go run ./internal/gentzindex

// portableTimezone validates names against the build's tzdata, never host files.
// Geographic backward links remain valid; aliases outside the areas do not.
func portableTimezone(name string) error {
	if name == "" {
		return nil
	}
	if name == "Local" {
		return invalid(`timezone "Local" depends on the hub's host; use an IANA name such as America/New_York, or UTC`)
	}
	if replacement, ok := timezoneReplacements[name]; ok {
		return invalid(fmt.Sprintf("timezone %q is not a portable IANA name; use %s", name, replacement))
	}
	if _, ok := bundledZones[name]; !ok {
		return invalid("invalid timezone")
	}
	if name == "UTC" || name == "Etc/UTC" {
		return nil
	}
	area, _, _ := strings.Cut(name, "/")
	switch area {
	case "Africa", "America", "Antarctica", "Arctic", "Asia", "Atlantic", "Australia", "Europe", "Indian", "Pacific":
		return nil
	}
	return invalid(fmt.Sprintf("timezone %q is not a portable IANA name; use an Area/Location name such as America/New_York, or UTC", name))
}

// cronDirectiveZone uses robfig's first ASCII-space boundary. A missing space
// is left to the existing parser/directive error path.
func cronDirectiveZone(expression string) (string, bool) {
	expression = strings.TrimSpace(expression)
	if !strings.HasPrefix(expression, "TZ=") && !strings.HasPrefix(expression, "CRON_TZ=") {
		return "", false
	}
	directive, _, ok := strings.Cut(expression, " ")
	if !ok {
		return "", false
	}
	_, zone, _ := strings.Cut(directive, "=")
	return zone, true
}

// Suggestions from tzdata 2026c's backward links, plus explicit fixed-offset
// readings for abbreviations. This map controls messages, not name validity.
var timezoneReplacements = map[string]string{
	"US/Alaska": "America/Anchorage", "US/Aleutian": "America/Adak",
	"US/Arizona": "America/Phoenix", "US/Central": "America/Chicago",
	"US/East-Indiana": "America/Indiana/Indianapolis", "US/Eastern": "America/New_York",
	"US/Hawaii": "Pacific/Honolulu", "US/Indiana-Starke": "America/Indiana/Knox",
	"US/Michigan": "America/Detroit", "US/Mountain": "America/Denver",
	"US/Pacific": "America/Los_Angeles", "US/Samoa": "Pacific/Pago_Pago",
	"Canada/Atlantic": "America/Halifax", "Canada/Central": "America/Winnipeg",
	"Canada/Eastern": "America/Toronto", "Canada/Mountain": "America/Edmonton",
	"Canada/Newfoundland": "America/St_Johns", "Canada/Pacific": "America/Vancouver",
	"Canada/Saskatchewan": "America/Regina", "Canada/Yukon": "America/Whitehorse",
	"EST5EDT": "America/New_York", "CST6CDT": "America/Chicago",
	"MST7MDT": "America/Denver", "PST8PDT": "America/Los_Angeles",
	"EST": "America/Panama for a fixed -05:00 offset, or America/New_York for New York time with daylight saving",
	"MST": "America/Phoenix for a fixed -07:00 offset, or America/Denver for Denver time with daylight saving",
	"HST": "Pacific/Honolulu",
	"CET": "Europe/Brussels", "MET": "Europe/Brussels", "EET": "Europe/Athens", "WET": "Europe/Lisbon",
	"GMT": "UTC", "GMT0": "UTC", "GMT+0": "UTC", "GMT-0": "UTC",
	"Greenwich": "UTC", "UCT": "UTC", "Universal": "UTC", "Zulu": "UTC",
	"Etc/GMT": "UTC", "Etc/GMT0": "UTC", "Etc/GMT+0": "UTC", "Etc/GMT-0": "UTC",
	"Etc/Greenwich": "UTC", "Etc/UCT": "UTC", "Etc/Universal": "UTC", "Etc/Zulu": "UTC",
}
