package cron

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSummaryTrigger(t *testing.T) {
	for _, trigger := range []string{"scheduled", "catchup", "request"} {
		s, err := ParseSummary([]byte(`{"version":1,"trigger":"` + trigger + `"}`))
		require.NoError(t, err)
		require.Equal(t, trigger, s.Trigger)
		require.True(t, s.UsesV2())
	}
	for _, raw := range []string{`null`, `""`, `"Scheduled"`, `"manual"`, `"unknown"`, `2`} {
		_, err := ParseSummary([]byte(`{"version":1,"trigger":` + raw + `}`))
		require.ErrorIs(t, err, ErrInvalid)
	}
}

func TestRunTrigger(t *testing.T) {
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		key     *string
		start   *time.Time
		want    string
		derived bool
	}{
		{nil, nil, "", false}, {new("request:"), nil, "", false}, {new("manual:"), nil, "", false},
		{new("request:run"), nil, "request", true}, {new("manual:run"), nil, "request", true},
		{new("Request:run"), nil, "", false}, {new("unsupported:run"), nil, "", false},
		{new("2026-11-03T01:00:00-08:00"), nil, "", false},
		{new("2026-11-03T01:00:00-08:00"), new(at.Add(120 * time.Second)), "scheduled", true},
		{new("2026-11-03T09:00:00Z"), new(at.Add(120*time.Second + time.Nanosecond)), "catchup", true},
		{new("2026-11-03T09:00:00Z"), new(at.Add(-time.Hour)), "scheduled", true},
		{new(strings.Repeat("x", 2048)), &at, "", false}, {new(string([]byte{255})), &at, "", false},
	} {
		got, derived := RunTrigger(Summary{Version: 1}, tc.key, tc.start)
		require.Equal(t, tc.want, got)
		require.Equal(t, tc.derived, derived)
		for _, explicit := range []string{"scheduled", "catchup", "request"} {
			got, derived = RunTrigger(Summary{Version: 1, Trigger: explicit}, tc.key, tc.start)
			require.Equal(t, explicit, got)
			require.False(t, derived)
		}
	}
}

func TestSummaryTriggerProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		trigger := []string{"scheduled", "catchup", "request"}[hegel.Draw(ht, hegel.Integers[int](0, 2))]
		original := Summary{Version: 1, Message: summaryPropertyText(ht, 100)}
		s := original
		s.Trigger = trigger
		require.True(ht, s.UsesV2())
		raw, err := json.Marshal(s)
		require.NoError(ht, err)
		parsed, err := ParseSummary(raw)
		require.NoError(ht, err)
		require.Equal(ht, s, parsed)
		again, err := json.Marshal(parsed)
		require.NoError(ht, err)
		require.Equal(ht, string(raw), string(again))
		parsed.Trigger = ""
		require.Equal(ht, original, parsed)
		require.False(ht, parsed.UsesV2())
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

// Generate keys by provenance class, so the oracle does not reuse RunTrigger's
// parsing branches. Timestamp cases include equivalent offset spellings.
func TestRunTriggerReferenceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		year := hegel.Draw(ht, hegel.Integers[int](0, 9999))
		month := time.Month(hegel.Draw(ht, hegel.Integers[int](1, 12)))
		nanos := hegel.Draw(ht, hegel.Integers[int](0, 999999999))
		at := time.Date(year, month, 15, 9, 0, 0, nanos, time.UTC)
		raw := hegel.Draw(ht, hegel.Text().MaxSize(2048))
		offset := hegel.Draw(ht, hegel.Integers[int64](-240000000000, 240000000001))
		started := new(at.Add(time.Duration(offset)))
		if hegel.Draw(ht, hegel.Booleans()) {
			started = nil
		}
		var key *string
		want, derived := "", false
		switch hegel.Draw(ht, hegel.Integers[int](0, 6)) {
		case 0:
			key = nil
		case 1:
			key = new("request:" + raw)
			if raw != "" {
				want, derived = "request", true
			}
		case 2:
			key = new("manual:" + raw)
			if raw != "" {
				want, derived = "request", true
			}
		case 3, 4:
			zone := time.FixedZone("fixture", hegel.Draw(ht, hegel.Integers[int](-1439, 1439))*60)
			key = new(at.In(zone).Format(time.RFC3339Nano))
			if started != nil {
				want, derived = "scheduled", true
				if offset > 120000000000 {
					want = "catchup"
				}
			}
		default:
			key = new("opaque:" + raw)
		}
		s := Summary{Version: 1}
		if hegel.Draw(ht, hegel.Booleans()) {
			s.Trigger = []string{"scheduled", "catchup", "request"}[hegel.Draw(ht, hegel.Integers[int](0, 2))]
			want, derived = s.Trigger, false
		}
		got, actualDerived := RunTrigger(s, key, started)
		require.Equal(ht, want, got)
		require.Equal(ht, derived, actualDerived)
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestDropReasonProperty(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"overlap_skipped", true}, {"catchup_missed", true}, {"paused_passed_over", true},
		{"failed", false}, {"", false}, {"overlap_skipped ", false},
		{" overlap_skipped", false}, {"catchup_missed\n", false}, {"PAUSED_PASSED_OVER", false},
	} {
		require.Equal(t, tc.want, DropReason(tc.code), "%q", tc.code)
	}
	codes := []string{ReasonOverlapSkipped, ReasonCatchupMissed, ReasonPausedPassedOver}
	for _, code := range codes {
		require.True(t, DropReason(code))
		for _, s := range []Summary{{Reason: code}, {Message: "reason: " + code + "\nDetails"}} {
			got, ok := RunReason(s)
			require.True(t, ok)
			require.Equal(t, code, got)
		}
	}
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.Text())
		want := code == "overlap_skipped" || code == "catchup_missed" || code == "paused_passed_over"
		require.Equal(ht, want, DropReason(code))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
