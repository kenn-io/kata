package cron

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// A cutoff advances only Through, never the interval anchor; advancing clears
// a frozen latest scan. A nonadvancing cutoff preserves the complete progress.
func TestApplyPassoverProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		anchor := time.Unix(hegel.Draw(ht, hegel.Integers[int64](0, 2000000000)), 0).UTC()
		through := anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(0, 100000))) * time.Second)
		cutoff := anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-100000, 200000))) * time.Second)
		p := RecurringProgress{Version: 1, Anchor: anchor, Through: through, Scan: &RecurringScan{DefinitionEventUID: definitionUID, Cursor: through.Add(time.Hour), Until: through.Add(2 * time.Hour)}}
		got, e := ApplyPassover(p, cutoff)
		require.NoError(ht, e)
		require.Equal(ht, anchor, got.Anchor)
		if cutoff.After(through) {
			require.Equal(ht, cutoff, got.Through)
			require.Nil(ht, got.Scan)
		} else {
			require.Equal(ht, p, got)
		}
		again, e := ApplyPassover(got, cutoff)
		require.NoError(ht, e)
		require.Equal(ht, got, again)
		require.NotNil(ht, p.Scan, "input remains unchanged")
	}, hegel.WithTestCases(1000))
}

// Compare complete bounded planner drains to one fixed horizon, rather than
// comparing individual batches whose pagination can differ after clamping.
func TestApplyPassoverPlanningProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		p := RecurringProgress{Version: 1, Anchor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		p.Through = p.Anchor
		j := nativeJob(ht)
		j.Trigger.IntervalSeconds = int64(hegel.Draw(ht, hegel.Integers(60, 3600)))
		if hegel.Draw(ht, hegel.Booleans()) {
			j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
		}
		horizon := p.Anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(1, 6000))) * time.Minute)
		cutoff := p.Anchor.Add(time.Duration(hegel.Draw(ht, hegel.Integers(-60, 6100))) * time.Minute)
		clamped, e := ApplyPassover(p, cutoff)
		require.NoError(ht, e)
		drain := func(job JobDefinition, progress RecurringProgress) []time.Time {
			var all []time.Time
			for range 10000 {
				b, err := PlanRecurring(job, definitionUID, progress, horizon)
				require.NoError(ht, err)
				all = append(all, b.Due...)
				progress = b.Progress
				if !b.More {
					return all
				}
			}
			ht.Fatal("planner did not drain")
			return nil
		}
		full := drain(j, p)
		var expected []time.Time
		for _, at := range full {
			if at.After(cutoff) {
				expected = append(expected, at)
			}
		}
		require.Equal(ht, expected, drain(j, clamped))
		for _, mode := range []string{"latest", "skip"} {
			j.Catchup = mode
			j.GraceSeconds = 60
			for _, at := range drain(j, clamped) {
				require.True(ht, at.After(cutoff))
			}
		}
	}, hegel.WithTestCases(1000))
}

func TestApplyPassoverInvalid(t *testing.T) {
	p := RecurringProgress{Version: 1, Anchor: at(t, "2026-01-01T00:00:00Z"), Through: at(t, "2026-01-01T00:00:00Z")}
	for _, cutoff := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))} {
		_, e := ApplyPassover(p, cutoff)
		require.ErrorIs(t, e, ErrInvalid)
	}
	p.Version = 0
	_, e := ApplyPassover(p, at(t, "2026-01-02T00:00:00Z"))
	require.ErrorIs(t, e, ErrInvalid)
}
