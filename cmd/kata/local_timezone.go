package main

import (
	"os"
	"strings"
	"time"
)

// localTimeZoneName names the CLI's local IANA zone: TZ first, then the
// /etc/localtime link. It returns "" when neither names a zone, so the daemon
// falls back to its own timezone.
func localTimeZoneName() string {
	return resolveLocalTimeZone(os.Getenv("TZ"), "/etc/localtime")
}

func resolveLocalTimeZone(tz, localtimePath string) string {
	if name := strings.TrimPrefix(strings.TrimSpace(tz), ":"); ianaZoneName(name) {
		return name
	}
	target, err := os.Readlink(localtimePath)
	if err != nil {
		return ""
	}
	const marker = "zoneinfo/"
	idx := strings.LastIndex(target, marker)
	if idx < 0 {
		return ""
	}
	if name := target[idx+len(marker):]; ianaZoneName(name) {
		return name
	}
	return ""
}

// ianaZoneName reports whether name loads as a zone. "Local" is excluded: it
// means the daemon's zone, not the caller's.
func ianaZoneName(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
