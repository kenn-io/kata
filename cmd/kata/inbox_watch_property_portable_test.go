//go:build inboxportable || !((linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64)))

package main

import (
	"testing"

	"pgregory.net/rapid"
)

// Hegel does not bundle a native engine for every platform Kata supports.
// Keep the identical properties on those hosts without requiring a local
// engine build. inboxportable lets hosted CI execute this runner on Linux.
type inboxPropertyT = rapid.T

func checkInboxProperty(t *testing.T, property func(*inboxPropertyT)) {
	t.Helper()
	// Twenty independent runs give 2,000 cases at Rapid's normal defaults,
	// preserving its reproduction flags without mutating shared flag state.
	for range 20 {
		rapid.Check(t, property)
	}
}

func inboxDrawText(t *inboxPropertyT) string { return rapid.String().Draw(t, "text") }
func inboxDrawInt64(t *inboxPropertyT) int64 { return rapid.Int64().Draw(t, "identity") }
func inboxDrawField(t *inboxPropertyT) int   { return rapid.IntRange(0, 5).Draw(t, "field") }
func inboxDrawBool(t *inboxPropertyT) bool   { return rapid.Bool().Draw(t, "flag") }
