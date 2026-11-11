package main

import (
	"go.kenn.io/kata/internal/recurrence/recurrenceclient"
)

// localTimeZoneName discovers the CLI host's local IANA zone.
var localTimeZoneName = func() string {
	return recurrenceclient.LocalZone(recurrenceclient.DefaultLocaltimePath)
}
