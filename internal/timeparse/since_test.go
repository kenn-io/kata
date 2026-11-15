package timeparse

import (
	"testing"
	"time"
)

func TestWindow(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 123, time.UTC)
	for _, tc := range []struct {
		name, since, until, wantSince, wantUntil string
		bad                                      bool
	}{
		{"maximum relative", "366d", "", now.Add(-366 * 24 * time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), false},
		{"relative explicit end", "24h", "2026-11-01T00:00:00Z", "2026-10-31T00:00:00Z", "2026-11-01T00:00:00Z", false},
		{"absolute without end", "2026-05-01T00:00:00Z", "", "2026-05-01T00:00:00Z", "", false},
		{"absolute explicit end", "2026-05-01T02:00:00+02:00", "2026-11-01T00:00:00Z", "2026-05-01T00:00:00Z", "2026-11-01T00:00:00Z", false},
		{"absent start", "", "", "", "", false},
		{"bad end", "7d", "bad", "", "", true},
		{"bad start", "garbage", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			since, until, err := Window(tc.since, tc.until, now)
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v", err)
			}
			if !tc.bad && (since != tc.wantSince || until != tc.wantUntil) {
				t.Fatalf("got %q, %q; want %q, %q", since, until, tc.wantSince, tc.wantUntil)
			}
		})
	}
}

func TestSince(t *testing.T) {
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		input string
		want  time.Time
		bad   bool
	}{
		{"24h", now.Add(-24 * time.Hour), false}, {"7d", now.Add(-7 * 24 * time.Hour), false},
		{"+1.5d", now.Add(-36 * time.Hour), false}, {"-0.5d", now.Add(12 * time.Hour), false},
		{"1h30m", now.Add(-90 * time.Minute), false},
		{"0.0000000000001d", now.Add(-8 * time.Nanosecond), false},
		{"-0.0000000000001d", now.Add(8 * time.Nanosecond), false},
		{"1.0000000000001d", now.Add(-24*time.Hour - 8*time.Nanosecond), false},
		{"0.00000000000025d", now.Add(-21 * time.Nanosecond), false},
		{".5d", now.Add(-12 * time.Hour), false}, {"1.d", now.Add(-24 * time.Hour), false},
		{"1h1d", time.Time{}, true}, {"1d1d", time.Time{}, true},
		{"2026-05-01T00:00:00Z", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), false},
		{"garbage", time.Time{}, true}, {"213504d", time.Time{}, true}, {"-213504d", time.Time{}, true},
		{"106751992d", time.Time{}, true}, {"-106751992d", time.Time{}, true}, {"-2562047h47m16.854775808s", time.Time{}, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Since(tc.input, now)
			if (err != nil) != tc.bad || (!tc.bad && !got.Equal(tc.want)) {
				t.Fatalf("got %v, %v; want %v, bad=%v", got, err, tc.want, tc.bad)
			}
		})
	}
}
