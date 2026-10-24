package jobform

import (
	"strings"
	"testing"
)

func TestRunAtLossless(t *testing.T) {
	for _, tc := range []struct{ stored, zone, display string }{
		{"2026-10-20T07:00:37.123Z", "Europe/Berlin", "2026-10-20 09:00:37.123 Europe/Berlin"},
		{"2026-10-25T02:30:00+01:00", "Europe/Berlin", "2026-10-25 02:30 Europe/Berlin"},
		{"2026-10-20T07:00:37.123Z", "Europe/London", "2026-10-20 08:00:37.123 Europe/London"},
	} {
		display, edit, err := RunAtText(tc.stored, tc.zone)
		if err != nil || display != tc.display || edit != tc.stored {
			t.Fatalf("display %q edit %q err %v", display, edit, err)
		}
		stored, err := ParseRunAt(edit, tc.stored, tc.zone)
		if err != nil || stored != tc.stored {
			t.Fatalf("round trip %q %v", stored, err)
		}
	}
	got, err := ParseRunAt("2026-10-25T02:30:00.123+01:00", "", "Europe/Berlin")
	if err != nil || got != "2026-10-25T01:30:00.123Z" {
		t.Fatalf("changed %q %v", got, err)
	}
}
func TestRunAtWallTransitions(t *testing.T) {
	for _, tc := range []struct{ input, zone, want, err string }{
		{"2026-03-29 02:30", "Europe/Berlin", "", "run at: 2026-03-29 02:30 does not exist in Europe/Berlin"},
		{"2026-10-25 02:30", "Europe/Berlin", "", "run at: 2026-10-25 02:30 is ambiguous in Europe/Berlin; add an offset, e.g. 2026-10-25T02:30:00+01:00"},
		{"2026-10-25T02:30:00+01:00", "Europe/Berlin", "2026-10-25T01:30:00Z", ""},
		{"2026-10-04 02:15", "Australia/Lord_Howe", "", "run at: 2026-10-04 02:15 does not exist in Australia/Lord_Howe"},
		{"2026-04-05 01:45", "Australia/Lord_Howe", "", "run at: 2026-04-05 01:45 is ambiguous in Australia/Lord_Howe; add an offset, e.g. 2026-04-05T01:45:00+10:30"},
		{"2026-10-20 09:00:37", "Europe/Berlin", "2026-10-20T07:00:37Z", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseRunAt(tc.input, "", tc.zone)
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Fatalf("%q %v", got, err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("%q %v", got, err)
			}
		})
	}
}
func TestSeconds(t *testing.T) {
	for _, tc := range []struct {
		in     string
		signed bool
		want   int64
	}{{"25h1m1s", false, 90061}, {"90061", false, 90061}, {"-1m", true, -60}, {"9223372036", false, 9223372036}, {"0", false, 0}} {
		n, err := ParseSeconds("duration", tc.in, tc.signed)
		if err != nil || n != tc.want {
			t.Fatalf("%s: %d %v", tc.in, n, err)
		}
	}
	if SecondsText(90061) != "25h1m1s" {
		t.Fatal(SecondsText(90061))
	}
	for _, in := range []string{"1.5s", "90.5", "1500ms"} {
		_, err := ParseSeconds("interval", in, false)
		if err == nil || err.Error() != "interval: whole seconds only" {
			t.Fatalf("%s: %v", in, err)
		}
	}
	for _, in := range []string{"9223372037", "9223372036854775807", "999999999999999999999999", "-1", "-1m", "", "oops"} {
		if _, err := ParseSeconds("interval", in, false); err == nil || !strings.HasPrefix(err.Error(), "interval:") {
			t.Fatalf("%s: %v", in, err)
		}
	}
}
