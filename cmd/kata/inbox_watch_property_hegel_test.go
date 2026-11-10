//go:build !inboxportable && ((linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64)))

package main

import (
	"math"
	"testing"

	"hegel.dev/go/hegel"
)

type inboxPropertyT = hegel.T

func checkInboxProperty(t *testing.T, property func(*inboxPropertyT)) {
	t.Helper()
	hegel.Test(t, property, hegel.WithTestCases(2000))
}

func inboxDrawText(t *inboxPropertyT) string { return hegel.Draw(t, hegel.Text()) }
func inboxDrawInt64(t *inboxPropertyT) int64 {
	return hegel.Draw(t, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
}
func inboxDrawField(t *inboxPropertyT) int { return hegel.Draw(t, hegel.Integers(0, 5)) }
func inboxDrawBool(t *inboxPropertyT) bool { return hegel.Draw(t, hegel.Integers(0, 1)) == 1 }
