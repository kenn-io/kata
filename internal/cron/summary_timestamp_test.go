package cron

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestSummaryStrictStepTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name, start, end string
		valid            bool
	}{
		{"zone hour", "2026-11-03T09:00:00+24:00", "", false},
		{"zone minute", "", "2026-11-03T09:00:00+00:60", false},
		{"single hour start", "2026-11-03T9:00:00Z", "", false},
		{"single hour end", "", "2026-11-03T9:00:00Z", false},
		{"comma fraction", "2026-11-03T09:00:00,1Z", "", false},
		{"calendar", "2026-02-30T09:00:00Z", "", false},
		{"reversed subnanosecond", "2026-11-03T09:00:00.0000000002Z", "2026-11-03T09:00:00.0000000001Z", false},
		{"reversed different lengths", "2026-11-03T09:00:00.12345678911Z", "2026-11-03T09:00:00.1234567891Z", false},
		{"reversed across zones", "2026-11-03T10:00:00.0000000002+01:00", "2026-11-03T09:00:00.0000000001Z", false},
		{"forward subnanosecond", "2026-11-03T09:00:00.0000000001Z", "2026-11-03T09:00:00.0000000002Z", true},
		{"equal trailing zeros", "2026-11-03T09:00:00.123456789100Z", "2026-11-03T09:00:00.1234567891Z", true},
		{"equal zero fraction", "2026-11-03T09:00:00.000000000000Z", "2026-11-03T09:00:00Z", true},
		{"equal across zones", "2026-11-03T10:00:00.000000000100+01:00", "2026-11-03T09:00:00.0000000001Z", true},
		{"next second", "2026-11-03T09:00:00.999999999999Z", "2026-11-03T09:00:01Z", true},
		{"maximum offset", "2026-11-03T09:00:00+23:59", "", true},
		{"unbounded precision", "2026-11-03T09:00:00." + strings.Repeat("1", 1024) + "Z", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"version":1,"steps":[{"key":"review","status":"running","started_at":%q,"ended_at":%q}]}`, tc.start, tc.end)
			summary, err := ParseSummary([]byte(raw))
			if !tc.valid {
				require.ErrorIs(t, err, ErrInvalid)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.start, summary.Steps[0].StartedAt)
			require.Equal(t, tc.end, summary.Steps[0].EndedAt)
			encoded, err := json.Marshal(summary)
			require.NoError(t, err)
			again, err := ParseSummary(encoded)
			require.NoError(t, err)
			require.Equal(t, summary, again)
		})
	}
}

func TestSummarySubnanosecondOrderingProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		start := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		end := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		raw := fmt.Sprintf(`{"version":1,"steps":[{"key":"review","status":"running","started_at":"2026-11-03T10:00:00.123456789%020d+01:00","ended_at":"2026-11-03T09:00:00.123456789%020dZ"}]}`, start, end)
		summary, err := ParseSummary([]byte(raw))
		if end < start {
			require.ErrorIs(ht, err, ErrInvalid)
			return
		}
		require.NoError(ht, err)
		encoded, err := json.Marshal(summary)
		require.NoError(ht, err)
		require.Equal(ht, raw, string(encoded))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}

func TestSummaryParseEnvelopeControls(t *testing.T) {
	for _, raw := range []string{
		strings.Repeat(" ", SummaryLimit) + `{"version":1}`,
		`{"version":1,"version":1}`, `{"version":1,"unknown":0}`,
		`{"version":1} {"version":1}`, `null`,
	} {
		_, err := ParseSummary([]byte(raw))
		require.ErrorIs(t, err, ErrInvalid)
	}
}
