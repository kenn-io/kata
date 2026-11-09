package tui

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/pkg/client/generated"
)

func TestCronNextPassover(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(3 * time.Minute)
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "adapter", true: "native"}[native], func(t *testing.T) {
			m := cronFixture(t)
			j := m.cron.data.jobs[0]
			j.Definition.Enabled = true
			if native {
				nativeNotify(generated.TriggerResponse{Kind: "interval", IntervalSeconds: new(int64(60))})(&j.Definition)
			} else {
				j.Definition.Trigger = generated.TriggerResponse{Kind: "cron", Cron: new("* * * * *"), Timezone: new("UTC")}
			}
			p := cronData{progress: map[string]cron.RecurringProgress{j.UID: {Version: 1, Anchor: now.Add(-time.Hour), Through: now.Add(-time.Hour)}}, parentOpen: map[string]bool{nextParentUID: true}, passovers: map[string]*db.CronPassoverState{j.UID: {Through: cutoff}}}
			_, lines := cronNextRuns(p, j, now)
			require.NotEmpty(t, lines)
			require.Contains(t, lines[0], "12:04")
			p.passovers[j.UID].Invalid = true
			_, lines = cronNextRuns(p, j, now)
			require.Equal(t, []string{"unavailable"}, lines)
		})
	}
}

// Adapter once previews use a strict after boundary, including equality.
func TestCronNextOncePassoverBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(time.Hour)
	for _, delta := range []time.Duration{-time.Second, 0, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			m := cronFixture(t)
			j := m.cron.data.jobs[0]
			j.Definition.Enabled = true
			j.Definition.Trigger = generated.TriggerResponse{Kind: "once", At: new(cutoff.Add(delta).Format(time.RFC3339Nano))}
			p := cronData{passovers: map[string]*db.CronPassoverState{j.UID: {Through: cutoff}}}
			_, lines := cronNextRuns(p, j, now)
			if delta <= 0 {
				require.Empty(t, lines)
			} else {
				require.Equal(t, []string{"2026-01-01 13:00:01 UTC"}, lines)
			}
		})
	}
}
