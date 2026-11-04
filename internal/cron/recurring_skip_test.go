package cron

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// A finalized catch-up window partitions its scheduled instants into children
// and discarded occurrences. Pending scans cannot establish that partition.
func TestRecurringSkipConservation(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(0, 2500))
		grace := hegel.Draw(ht, hegel.Integers(1, 3600))
		calendar := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		policy := hegel.Draw(ht, hegel.SampledFrom([]string{"all", "skip", "latest"}))
		j := nativeJob(t)
		j.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
		if calendar {
			j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
		}
		j.Catchup, j.GraceSeconds = policy, int64(grace)
		anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
		now := anchor.Add(time.Duration(n)*time.Minute + 17*time.Second)
		delivered, skipped, capped := 0, 0, false
		for steps := range 100 {
			b, err := PlanRecurring(j, definitionUID, p, now)
			if err != nil {
				ht.Fatal(err)
			}
			delivered += len(b.Due)
			skipped += b.Skipped.Count
			capped = capped || b.Skipped.Capped
			if b.Progress.Scan != nil && b.Skipped.Count != 0 {
				ht.Fatal("pending scan reported finalized skips")
			}
			p = b.Progress
			if !b.More {
				break
			}
			if steps == 99 {
				ht.Fatal("planner failed to finish bounded window")
			}
		}
		wantDelivered := n
		switch policy {
		case "latest":
			if n > 0 {
				wantDelivered = 1
			}
		case "skip":
			wantDelivered = 0
			for i := 1; i <= n; i++ {
				if !anchor.Add(time.Duration(i) * time.Minute).Before(now.Add(-time.Duration(grace) * time.Second)) {
					wantDelivered++
				}
			}
		}
		if delivered != wantDelivered {
			ht.Fatalf("delivered=%d want=%d", delivered, wantDelivered)
		}
		wantSkipped := n - wantDelivered
		if (!capped && skipped != wantSkipped) || (capped && (skipped > wantSkipped || skipped < 1000)) {
			ht.Fatalf("skipped=%d capped=%v want=%d", skipped, capped, wantSkipped)
		}
	}, hegel.WithTestCases(1000))
}

func TestRecurringSkipGraceBoundary(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
	j.Catchup = "skip"
	j.GraceSeconds = 60
	anchor := at(t, "2026-01-01T00:00:00Z")
	b, e := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, anchor.Add(3*time.Minute))
	require.NoError(t, e)
	require.Equal(t, []time.Time{anchor.Add(2 * time.Minute), anchor.Add(3 * time.Minute)}, b.Due)
	require.Equal(t, 1, b.Skipped.Count)
	require.Equal(t, anchor, b.Skipped.After)
	require.Equal(t, anchor.Add(2*time.Minute), b.Skipped.Before)
}

func TestRecurringLatestSkipFinalization(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "cron", Cron: "* * * * *"}
	j.Catchup = "latest"
	anchor := at(t, "2026-01-01T00:00:00Z")
	p := RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}
	now := anchor.Add(1500 * time.Minute)
	first, e := PlanRecurring(j, definitionUID, p, now)
	require.NoError(t, e)
	require.True(t, first.More)
	require.Zero(t, first.Skipped.Count)
	final, e := PlanRecurring(j, definitionUID, first.Progress, now.Add(500*time.Minute))
	require.NoError(t, e)
	require.Equal(t, []time.Time{now}, final.Due)
	require.Equal(t, now, final.Progress.Through)
	require.Equal(t, 1000, final.Skipped.Count)
	require.True(t, final.Skipped.Capped)
	// A policy change restarts the scan; previously scanned instants can be delivered.
	j.Catchup = "all"
	restart, e := PlanRecurring(j, definitionUID, first.Progress, now)
	require.NoError(t, e)
	require.Len(t, restart.Due, 50)
	require.Zero(t, restart.Skipped.Count)
	j.Catchup = "latest"
	rollback, e := PlanRecurring(j, definitionUID, first.Progress, now.Add(-time.Minute))
	require.NoError(t, e)
	require.True(t, rollback.More)
	require.Zero(t, rollback.Skipped.Count)
	revision, e := PlanRecurring(j, "01ARZ3NDEKTSV4RRFFQ69G5FAX", first.Progress, now)
	require.NoError(t, e)
	require.True(t, revision.More)
	require.Zero(t, revision.Skipped.Count)
}

// This deterministic 4,320-occurrence window exceeds the property's 2,500
// maximum and catches accidental caps on longer interval backlogs.
func TestRecurringSkipIntervalCountIsExact(t *testing.T) {
	j := nativeJob(t)
	j.Trigger = Trigger{Kind: "interval", IntervalSeconds: 60}
	j.Catchup, j.GraceSeconds = "skip", 1
	anchor := at(t, "2026-01-01T00:00:00Z")
	b, err := PlanRecurring(j, definitionUID, RecurringProgress{Version: 1, Anchor: anchor, Through: anchor}, anchor.Add(72*time.Hour+2*time.Second))
	require.NoError(t, err)
	require.Equal(t, 4320, b.Skipped.Count)
	require.False(t, b.Skipped.Capped)
	require.Empty(t, b.Due)
}
