package main

import (
	"os"

	"go.kenn.io/kata/internal/recurrence"
)

// localtimePath is the system zone link; tests override it.
var localtimePath = recurrence.DefaultLocaltimePath

// localTimeZoneName names the CLI's local IANA zone: TZ first, then the
// /etc/localtime link. It returns "" when nothing names a zone, so the daemon
// falls back to its own timezone.
func localTimeZoneName() string {
	tz, set := os.LookupEnv("TZ")
	return resolveLocalTimeZone(tz, set, localtimePath)
}

var resolveLocalTimeZone = recurrence.ResolveLocalZone
