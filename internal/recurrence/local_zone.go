package recurrence

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalZone names the caller's local IANA zone: TZ first, then the
// link at localtimePath. It returns "" when nothing names a zone, so the
// daemon falls back to its own timezone.
func LocalZone(localtimePath string) string {
	tz, set := os.LookupEnv("TZ")
	return ResolveLocalZone(tz, set, localtimePath)
}

// ResolveLocalZone is LocalZone with its inputs given. A TZ that is set but
// empty means UTC, as in POSIX and Go.
func ResolveLocalZone(tz string, tzSet bool, localtimePath string) string {
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
	// Windows stores link targets with backslashes.
	_, name, found := strings.CutLast(filepath.ToSlash(target), "zoneinfo/")
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
