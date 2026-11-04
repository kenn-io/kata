package tui

import (
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"hegel.dev/go/hegel"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/cron"
)

func TestCronSummaryTriggerDetail(t *testing.T) {
	for _, tc := range []struct{ name, explicit, key, reason, message, wantTrigger, wantReason string }{
		{"explicit", "request", "2026-05-01T09:00:00Z", "failed", "", "request", "failed"},
		{"derived", "", "2026-05-01T09:00:00Z", cron.ReasonOverlapSkipped, "reason: failed", "catchup (derived)", "overlap_skipped (not launched)"},
		{"scheduled", "", "2026-05-01T10:00:00Z", "", "reason: " + cron.ReasonCatchupMissed, "scheduled (derived)", "catchup_missed (not launched)"},
		{"request", "", "request:run", cron.ReasonPausedPassedOver, "", "request (derived)", "paused_passed_over (not launched)"},
		{"unknown", "", "opaque:run", "failed", "", "", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := cronSummaryFixture(t)
			m.width = 100
			run := &m.cron.data.runs[0]
			run.OccurrenceKey = &tc.key
			run.Summary.Trigger = optionalSummaryTime(tc.explicit)
			run.Summary.Reason = optionalSummaryTime(tc.reason)
			run.Summary.Message = optionalSummaryTime(tc.message)
			_, _, lines := cronDetail(m, cronRef{cronRuns, run.UID})
			var b strings.Builder
			for _, line := range lines {
				b.WriteString(line.text)
				b.WriteByte('\n')
			}
			text := b.String()
			if tc.wantTrigger == "" {
				require.NotContains(t, text, "Trigger:")
			} else {
				require.Contains(t, text, "Trigger:    "+tc.wantTrigger)
			}
			require.Contains(t, text, "Reason:     "+tc.wantReason)
			if tc.wantTrigger != "" {
				require.Less(t, strings.Index(text, "Occurrence:"), strings.Index(text, "Trigger:"))
				require.Less(t, strings.Index(text, "Trigger:"), strings.Index(text, "Started:"))
			}
		})
	}
}

func TestCronSummaryTriggerDetailProperty(t *testing.T) {
	m := cronSummaryFixture(t)
	cases := []struct{ explicit, key, trigger, reason string }{
		{"request", "2026-05-01T09:00:00Z", "request", "failed"},
		{"", "2026-05-01T09:00:00Z", "catchup (derived)", cron.ReasonOverlapSkipped},
		{"", "2026-05-01T10:00:00Z", "scheduled (derived)", cron.ReasonCatchupMissed},
		{"", "manual:run", "request (derived)", cron.ReasonPausedPassedOver},
		{"", "opaque:run", "", "failed"},
	}
	compact := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, s)
	}
	hegel.Test(t, func(ht *hegel.T) {
		tc := cases[hegel.Draw(ht, hegel.Integers[int](0, len(cases)-1))]
		m.width = hegel.Draw(ht, hegel.Integers[int](20, 120))
		run := &m.cron.data.runs[0]
		run.OccurrenceKey, run.Summary.Trigger, run.Summary.Reason = &tc.key, optionalSummaryTime(tc.explicit), optionalSummaryTime(tc.reason)
		_, _, lines := cronDetail(m, cronRef{cronRuns, run.UID})
		var b strings.Builder
		for _, line := range lines {
			if ansi.StringWidth(line.text) > m.width-2 {
				ht.Fatalf("overflow at width %d: %q", m.width, line.text)
			}
			b.WriteString(line.text)
		}
		text := compact(b.String())
		if tc.trigger == "" {
			if strings.Contains(text, "Trigger:") {
				ht.Fatalf("unknown trigger displayed: %q", text)
			}
		} else if !strings.Contains(text, "Trigger:"+compact(tc.trigger)) {
			ht.Fatalf("trigger %q missing from %q", tc.trigger, text)
		}
		reason := tc.reason
		if tc.reason != "failed" {
			reason += " (not launched)"
		}
		if !strings.Contains(text, "Reason:"+compact(reason)) {
			ht.Fatalf("reason %q missing from %q", reason, text)
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
