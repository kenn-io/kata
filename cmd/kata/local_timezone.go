package main

import (
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
)

// localtimePath is the system zone link; tests override it.
var localtimePath = recurrenceclient.DefaultLocaltimePath

// localTimeZoneName names the CLI's local IANA zone: TZ first, then the
// /etc/localtime link. It returns "" when nothing names a zone, so the daemon
// falls back to its own timezone.
func localTimeZoneName() string {
	return recurrenceclient.LocalZone(localtimePath)
}

var resolveLocalTimeZone = recurrenceclient.ResolveLocalZone
