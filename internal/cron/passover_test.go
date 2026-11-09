package cron

import (
	"flag"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// A cutoff advances only Through, never the interval anchor; advancing clears
// a frozen latest scan. A nonadvancing cutoff preserves the complete progress.
func TestApplyPassover(t *testing.T) {
	anchor, through := at(t, "2026-01-01T00:00:00Z"), at(t, "2026-01-01T06:00:00Z")
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: through, Scan: &RecurringScan{DefinitionEventUID: definitionUID, Cursor: through.Add(time.Hour), Until: through.Add(2 * time.Hour)}}
	for _, cutoff := range []time.Time{anchor.Add(-time.Hour), anchor, through} {
		got, e := ApplyPassover(p, cutoff)
		require.NoError(t, e)
		require.Equal(t, p, got)
	}
	got, e := ApplyPassover(p, through.Add(time.Second))
	require.NoError(t, e)
	require.Equal(t, RecurringProgress{Version: 1, Anchor: anchor, Through: through.Add(time.Second)}, got)
	require.NotNil(t, p.Scan, "input remains unchanged")

	for _, cutoff := range []time.Time{{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))} {
		_, e := ApplyPassover(p, cutoff)
		require.ErrorIs(t, e, ErrInvalid)
	}
	p.Version = 0
	_, e = ApplyPassover(p, at(t, "2026-01-02T00:00:00Z"))
	require.ErrorIs(t, e, ErrInvalid)
}

// Compare complete bounded planner drains to one fixed horizon, rather than
// comparing individual batches whose pagination can differ after clamping.
func TestApplyPassoverPlanningProperty(t *testing.T) {
	base := nativeJob(t)
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	require.NoError(t, flag.Set("rapid.checks", "1000"))
	defer func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) }()
	rapid.Check(t, func(ht *rapid.T) {
		p := RecurringProgress{Version: 1, Anchor: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		p.Through = p.Anchor
		j := base
		j.Trigger.IntervalSeconds = int64(rapid.IntRange(60, 3600).Draw(ht, "interval seconds"))
		if rapid.Bool().Draw(ht, "cron trigger") {
			j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
		}
		horizon := p.Anchor.Add(time.Duration(rapid.IntRange(1, 6000).Draw(ht, "horizon")) * time.Minute)
		cutoff := p.Anchor.Add(time.Duration(rapid.IntRange(-60, 6100).Draw(ht, "cutoff")) * time.Minute)
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
	})
}
