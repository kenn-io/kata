package tui

import (
	"flag"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"
)

func cronRapidCheck(t *testing.T, checks int, fn func(*rapid.T)) {
	t.Helper()
	checksFlag := flag.Lookup("rapid.checks")
	if checksFlag == nil {
		t.Fatal("rapid.checks flag is not registered")
	}
	previous := checksFlag.Value.String()
	if err := flag.Set("rapid.checks", strconv.Itoa(checks)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := flag.Set("rapid.checks", previous); err != nil {
			t.Errorf("restore rapid.checks: %v", err)
		}
	}()
	rapid.Check(t, fn)
}

func cronDateTimes() *rapid.Generator[time.Time] {
	return rapid.Custom(func(rt *rapid.T) time.Time {
		seconds := rapid.Int64Range(-62135596800, 253402300799).Draw(rt, "Unix seconds")
		nanos := rapid.IntRange(0, 999_999_999).Draw(rt, "nanoseconds")
		return time.Unix(seconds, int64(nanos)).UTC()
	})
}
