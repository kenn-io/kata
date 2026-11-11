package main

import (
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
)

// localtimePath is the system zone link; tests override it.
var localtimePath = recurrenceclient.DefaultLocaltimePath

// localTimeZoneName names the CLI's local IANA zone, or returns "" if unknown.
var localTimeZoneName = func() string {
	return recurrenceclient.LocalZone(localtimePath)
}
