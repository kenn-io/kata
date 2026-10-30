package main

import (
	"os"
	"strings"
	"time"
)

// localtimePath is the system zone link; tests override it.
var localtimePath = "/etc/localtime"

// localTimeZoneName names the CLI's local IANA zone: TZ first, then the
// /etc/localtime link. A TZ that is set but empty means UTC, as in POSIX and
// Go. It returns "" when nothing names a zone, so the daemon falls back to its
// own timezone.
func localTimeZoneName() string {
	tz, set := os.LookupEnv("TZ")
	return resolveLocalTimeZone(tz, set, localtimePath)
}

func resolveLocalTimeZone(tz string, tzSet bool, localtimePath string) string {
	if tzSet && strings.TrimSpace(tz) == "" {
		return "UTC"
	}
	if name := strings.TrimPrefix(strings.TrimSpace(tz), ":"); isIANAZone(name) {
		return name
	}
	target, err := os.Readlink(localtimePath)
	if err != nil {
		return ""
	}
	_, name, found := strings.CutLast(target, "zoneinfo/")
	if found && isIANAZone(name) {
		return name
	}
	return ""
}

// isIANAZone reports whether name loads as a zone. "Local" is excluded: it
// means the daemon's zone, not the caller's.
func isIANAZone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
